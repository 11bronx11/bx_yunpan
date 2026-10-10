# Asynq 迁移 RabbitMQ：执行计划

日期：2026-09-21。状态：待实施。本轮已完成源码评估和针对性基线检查，下面的开发、迁移、演练均未执行。

背景与文件清单：[RABBITMQ_MIGRATION_ASSESSMENT.md](RABBITMQ_MIGRATION_ASSESSMENT.md)。本计划以当前工作区为基线，独立于历史架构升级计划。

## 1. 目标、范围与完成标准

目标：使用 RabbitMQ 承担现有 Asynq 的 object、media、maintenance、ai、search 任务，保留业务事务与 Outbox 的一致性、任务终态、故障恢复、可观测性以及可执行的回滚路径。

默认采用“兼容版本先部署，新任务切换，旧任务排空”的方式。开发环境若证实没有历史任务，可以跳过旧队列排空，但不能据此省略发布确认、消费重试和业务幂等验证。

范围包括原合并 `worker/aisvc` 和拆分角色。Kafka 事件流、缓存/限流 Redis、对外 HTTP/gRPC 协议、前端及搜索底层选型均保留。

最终完成必须同时满足：

- 五类业务队列已在 RabbitMQ 执行，功能和状态收敛验证通过。
- confirm、重投、延迟、最终失败、超时、重连和停机测试通过。
- 旧 Asynq 未完结任务有完整清单和归属，需执行任务已排空，历史归档任务有处置结论。
- 恢复和重放不会绕过永久失败规则或重置 GC 等待时间。
- 新旧指标与告警交接完成，回滚已在隔离环境演练。
- 兼容期结束后，生产路径移除 Asynq 和 Redis Broker 依赖；Redis Cache/Coordination 继续工作。
- 队列数据、数据库和卷的清理由单独的清理清单决定，不作为自动迁移步骤。

## 2. 实施前冻结的设计

| 决策 | 计划默认值 | 原因 |
| --- | --- | --- |
| Broker 协议/客户端 | AMQP 0-9-1 / amqp091-go，固定版本 | 有成熟客户端，适合当前 Go handler 模型 |
| 队列 | 五类 durable quorum 工作队列 | 对齐当前角色边界 |
| 重试 | 显式计数 + 固定 TTL retry queues + 可靠 DLX | 保留应用重试上限与终态回调；可跨已验证的 RabbitMQ 4.x 版本 |
| GC 长延迟 | PostgreSQL 持久化 `not_before`，到期发布 | 不依赖延迟插件，便于故障恢复和迁移审计 |
| 重复处理 | 业务状态机/唯一约束/租约 + 消费记录 | 普通 RabbitMQ queue 不按 MessageId 去重 |
| 发布状态 | Broker 接受且可路由后才 published | 保留 Outbox 语义 |
| 切换 | 新任务 RabbitMQ、旧任务 Asynq 排空 | 保留 scheduled/retry/active 状态 |
| 回滚 | 回到兼容版本，保留可消费两个后端的能力 | 已确认到 RabbitMQ 的任务不能靠改开关返回 Redis |
| 联调 Broker | 隔离环境或现有 Broker 的独立 vhost | 不复用未盘点的队列；本轮尚未创建 |

RabbitMQ 4.3 原生 delayed retry 是备选优化，不能同时与 TTL 方案叠加，否则可能双重延迟。本轮估算以 TTL 方案为准。P0 如选原生方案，需同步改写重试协议和验收用例。

### 2.1 任务契约

新增 Broker 无关的任务接口，保持窄边界：

```go
// 示意接口，字段和签名在 P0 冻结后实施。
type Task struct {
    MessageID   string
    EventID     string
    Type        string
    Destination string
    Payload     []byte
    RetryCount  int
    MaxRetry    int
    Timeout     time.Duration
}

type Handler func(context.Context, Task) error

type Publisher interface {
    Publish(context.Context, Task) error // 仅在确认接收且可路由后返回 nil
}
```

生命周期管理与任务契约分开；业务 handler 不持有 RabbitMQ connection、channel、delivery tag。保留 Asynq 适配层直至旧任务排空。

`MessageID` 与业务 payload 的 `task_id` 必须分开；事件 ID、逻辑 destination 和 retry metadata 在重发时稳定、可追踪。JSON schema 不同时进行大规模重构，新增 header 不覆盖业务字段。

### 2.2 三套独立计数和时间

| 项目 | 含义 | 禁止混用 |
| --- | --- | --- |
| Outbox `attempt` | Relay 发布尝试次数 | 不能用作业务 handler 重试次数 |
| `RetryCount` | 一条任务执行重试链中已用重试数，初始 0 | 不用 Broker redelivered 布尔值替代 |
| Broker delivery-count / x-death | 传输重投、死信历史 | 随 Broker 版本/路径变化，不能直接作为 MaxRetry |
| `not_before` | 业务最早可执行时间，GC 等待下限 | 发布失败不能提前执行或每次重新延迟 24h |
| `available_at` | 当前 delivery 下一次允许发布的时间 | 必须不早于 not_before |
| handler timeout | 一次业务执行时限 | 不等于 Broker 消费确认超时 |

默认 `MaxRetry=8` 指首次加最多 8 次执行重试。对断线后的重复投递、并发重复和第三方调用不承诺全局恰好执行 9 次；业务副作用仍由状态机和 fencing 保护。

### 2.3 数据库演进

仅新增 migration，编号以实施时仓库状态为准：

1. 增加可空/可回填的 delivery `not_before`；新 GC 首次由 Relay 调度时，持久化该时刻加配置延迟，再等到期发布。调度重启不得重新起算，不能直接按旧事件创建时间使任务提前执行。旧 scheduled 消息由 Asynq 保持原时间。
2. 为 RabbitMQ claim/reclaim 查询增加索引；保留 Asynq 索引支持排空和回滚。预先评估 DDL 锁和回填量，在线大表索引需使用迁移工具支持的非事务方案。
3. 增加终态失败留存，例如 `task_failures`：事件、canonical destination、业务资源/版本、错误分类、重试信息、payload 引用、处置状态、重放审计。它服务于 DLQ 与 Recovery 收敛，不另建泛用工作流引擎。
4. 迁移/重放工具需要持久审计映射，记录原 transport、任务 ID、状态、到期时间、已用重试和新身份；若采用自然排空，记录归属和排空报告即可，不额外搬运消息。
5. 所有 delivery 创建入口按 `(event_id, canonical_destination)` 判断执行归属，识别旧别名与两个任务 transport；以事务锁或经历史数据审计后建立的约束避免双目标创建。原主键包含 transport，本身不能阻止两种 Broker 各写一行。

禁止把全部历史 published 行重新标 pending。更新 legacy event 总状态、补投和恢复查询时，必须正确理解新旧 transport 和终态失败记录。

### 2.4 配置与运行模式

计划新增名称，以下均尚未实现：

| 配置 | 职责 |
| --- | --- |
| `TASK_PUBLISH_BACKEND=asynq|rabbitmq` | 决定新 Outbox delivery 的任务 transport；所有生产进程一致 |
| `TASK_CONSUME_BACKEND=asynq|rabbitmq` | 当前消费者读哪个 Broker；支持旧任务排空进程 |
| `TASK_RELAY_TRANSPORT=asynq|rabbitmq` | Relay 只领取哪个 transport；合并入口也要显式处理 |
| `OUTBOX_LEADER_LOCK_KEY` | 按 transport 派生不同锁名；同后端副本共享锁，两个后端不能争同一把锁 |
| `RABBITMQ_URL` 或结构化连接项 | 包含 vhost 的连接信息，禁止原样记录含凭据的 URL |
| `RABBITMQ_PUBLISH_TIMEOUT` / 在途窗口 | 发布等待上限、背压与有界内存 |
| `RABBITMQ_PREFETCH` / 每队列并发预算 | 限制 unacked 和业务并发；启动时校验预算总和 |
| `WORKER_SHUTDOWN_TIMEOUT` | 有限优雅停机窗口，与容器 stop_grace_period 配合 |

沿用 `OUTBOX_TASK_MAX_RETRY`、`OUTBOX_TASK_TIMEOUT`、`OBJECT_GC_DELAY` 的业务意义。连接参数、TLS、心跳、vhost、权限、Broker consumer-timeout、dead-letter-strategy 和 delivery-limit 都要进入版本化部署配置。

API 和 ai-query 不应因为 RabbitMQ 故障而失去只依赖业务主库的核心就绪状态；异步平面以 advisory health 和积压告警暴露。Relay/Worker 的就绪必须反映可发布/可消费状态，不能只证明进程存活。

## 3. 分阶段执行与验收

状态约定：本轮仅完成“评估基线”；P0-P6 全部待开始。

| 阶段 | 人日 | 交付物 | 退出条件 |
| --- | --- | --- | --- |
| P0 冻结契约与队列盘点 | 1-2 | 接口、拓扑、时间/重试语义、部署方式、旧任务分类清单 | 任务类型全覆盖，切换和回滚的数据归属明确 |
| P1 解耦 Asynq 与联动修复 | 2-3 | taskqueue 契约、Asynq 适配、handler 改造、task_id 修复 | 原 Asynq 功能和兼容测试保持通过 |
| P2 RabbitMQ 发布和消费核心 | 3-4 | confirm/return、消费池、重连、手动 ACK、停机 | 最小故障矩阵和所有角色装配通过 |
| P3 延迟、重试、终态与 Outbox 演进 | 3-4 | 新迁移、TTL/DLX、GC 到期、失败记录、Recovery 联动 | 延迟不提前、失败不丢失、终态可收敛 |
| P4 部署、观测与兼容工具 | 2-3 | Compose/外部 Broker 模式、指标告警、盘点/交接工具 | 合并/拆分/no-AI/search 开关组合可启动并可观测 |
| P5 隔离联调和切换/回滚演练 | 3-5 | 故障报告、业务回归、预算测试、交接与回滚记录 | 下文必须项通过，积压/失败去向可解释 |
| P6 受控切换和旧路径退役 | 2-3 | 实际切换记录、排空报告、Asynq 依赖退役变更 | 完成标准全部达成，观察期结束 |
| 合计 | 16-24 | 可交付迁移版本 | 不包含外部审批/等待及额外 HA 工程 |

阶段可在文件和职责清楚时交错推进，但 P3 可靠性完成前不能按 P2 的“能收发”结果宣布可上线。

### P0：冻结与盘点

- 基于当日工作区重新确认路由、已有未提交改动和所有事件生产者。
- 只读统计 outbox transport/status/destination、最旧未发布记录；列出各 Asynq queue 的 pending/active/scheduled/retry/archived/aggregating/completed。
- 明确旧 GC 的最晚到期时间、最长 AI 任务、需要人工处理的 archive 数量。
- 固定 RabbitMQ/客户端版本，确认独立 vhost、网络、权限、资源、持久卷和部署所有权。
- 核验 `task_id` 问题并先补最小回归用例；记录旧队列 payload 是否已受影响，必要时按原 Outbox payload 校正重放。
- 固定并发预算与失败处置协议，确定开发环境是否可以无旧任务直接切换。

### P1：保持 Asynq 行为完成解耦

- 将业务 handler 迁移到通用 Task/Handler，保留旧任务类型别名。
- 用 Asynq 适配层注入 retry/maxRetry/taskID/trace，实现通用错误分类到 SkipRetry 的映射。
- 改造合并和拆分启动入口；保障 BaseContext 中 V2/Search/Kafka 和新发布 transport 标志继续传到消费过程中产生的业务事件。
- 业务 task_id 不再被 relay 的传输元数据覆盖；原搜索 envelope 元数据保持可读。
- 在原后端完成上传、媒体、GC、AI 重建及 search 路由回归，形成可回滚的兼容版本。

### P2：实现 RabbitMQ 传输

- 声明 versioned exchange、quorum queue 和 bindings，参数不一致时清晰失败，不能自动删除重建现有队列。
- 采用独立的发布/消费资源管理，跟踪 confirm、return、连接关闭和未决发布。
- 实现有界 worker pool、QoS、panic 恢复、手动单条 ACK，以及同一 channel 的 ACK 归属。
- 重连后重建需要的连接状态、拓扑、QoS、订阅与消费者就绪；处理 Broker blocked/unblocked 和 consumer cancel。
- 校验 Outbox claim 租期相对 publish timeout、在途量和批次时长足够；必要时独立配置 publish lease，避免沿用 30m 任务时限导致长时间停顿。
- 先用对象校验链路验证，再接全角色；此阶段仍默认 Asynq。

### P3：补齐任务执行语义

- 实现固定档位 retry queues 和可靠 DLX，校验最大重试为 0 和默认 8 的边界。
- retry publish confirm 成功后才 ACK 原消息；永久失败/耗尽先持久化审计和业务终态，再可靠进入 DLQ。
- 终态处理失败有可恢复记录和幂等重试入口；Broker 自身 delivery-limit 进入 DLQ 时也可追踪到业务任务。
- 追加 GC 到期字段及 Outbox 索引，完成旧数据回填和回滚验证。
- Recovery 识别尚未到期任务、已有有效处理、永久失败和人工重放状态。不能把 30m 正常执行机械当成 15m 后的丢失任务。
- 更新 `ensureLegacyDeliveries`、`ensureSearchDeliveries`、`EnsureRecovery` 与共享创建函数，防止过渡期间补出另一套 transport。
- Relay 选主锁按 transport 隔离，验证两个后端都有持续投递能力；同一后端多副本仍使用相同锁名。

### P4：部署、监控和工具

- 支持受版本控制的 Compose Broker 与外部 Broker 连接两种配置入口；默认不争用现有容器的宿主端口。
- 开发可用单节点 quorum；生产多节点单列验证，资源不足时不同时运行两套大型基础设施压测。
- 迁移 Asynq Inspector 指标；保留业务 handler 成功/失败/耗时/在途指标，增加 RabbitMQ ready、unacked、retry、DLQ、confirm latency、return、reconnect、consumer count。
- Outbox 指标覆盖新旧 transport；GC 正常等待与“已到期但未发布”分别告警，避免 24h 定时任务被长期误报。
- 提供只读 inventory、只操作指定任务的 replay/transfer、dry-run、可续跑审计及回滚映射。所有重放默认限速且保留原到期时间。
- 不用管理 UI 中的批量 purge/delete 代替交接；原始业务 payload 与凭据不写入普通日志。

### P5：隔离环境验证

- 使用独立 PostgreSQL 数据库、Redis 命名空间/实例、RabbitMQ vhost 和对象存储前缀，先证实隔离，再运行写入/故障测试。
- 顺序执行数据库迁移验证、各角色并发预算、staging 全链路、最后容量和恢复时间测试。
- 采用 fake AI 验证业务状态和重复处理；真实供应商联调另记，不能用 fake AI 数据承诺生产模型吞吐。
- 用两份相同数据/配置对比 Asynq 和 RabbitMQ 的任务完成耗时、积压恢复速度、CPU/内存、数据库连接和 I/O，记录吞吐收益或回退。
- 完成一次正向切换、一次切换前回退、一次 RabbitMQ 已有确认消息后的回滚。

### P6：切换与退役

按下一节 runbook 执行。观察窗建议至少覆盖一个默认 GC 周期，即 24h，并覆盖最长重试链和一次任务峰值；这只是最小观察建议，不能替代业务发布约定。

## 4. 切换 Runbook

以下是待实施程序，不能直接把步骤当成已经可运行的脚本。实施 P4 时需落成带检查和审计的命令。

### 4.1 切换前

1. 在所有 API、合并/拆分 Worker、AISvc/AI Indexer、Recovery 及运维事件生产工具上部署兼容版本；发布和消费继续 Asynq。
2. 新增数据库字段和索引，保留旧结构；确认旧版本所需语义仍可用。对数据库与队列元数据准备可验证的备份/导出方案。
3. 建好 RabbitMQ 隔离拓扑、权限、告警和消费者，测试专用消息通过后确认业务队列空或已有清晰的归属。
4. 输出 Asynq 全状态 inventory、Outbox inventory 和人工归档处置清单。archive 不计入需要自然执行的 backlog，但不能假装不存在。

### 4.2 路由切换屏障

1. 暂停新业务写入及后台恢复/清理生产，停止 relay 和消费者的新领取；让活跃事务结束并记录切换边界。长任务有限等待后取消并交回未确认消息，确保携带旧发布配置的进程退出。窗口时长取决于实际停机预算，必须同时覆盖所有会写 Outbox 的进程。
2. 统一将新事件写 transport 切为 RabbitMQ，确保旧消费排空进程也携带新发布策略，因为它们处理完上传等任务后会产生新事件。
3. 已存在 delivery 的 transport 保持不变；兼容补投按既有逻辑目的地识别归属。禁止将历史 published 行全部复制到 RabbitMQ。
4. 处理仍处于 sending 的 claim：持有者停止或租期到期后由原 transport relay 接管，不能在发布结果未知时直接搬到另一个 Broker。
5. 恢复新业务写入、Recovery，以及分别处理 Asynq/RabbitMQ delivery 的 relay；核对两套 relay 使用不同 transport 的选主锁。

### 4.3 并行排空

1. RabbitMQ 消费新任务；使用兼容版本的 Asynq 消费者继续处理旧 ready、active、retry、scheduled。
2. 两套消费者的总并发、数据库连接池和外部服务预算合并计算，不能因为迁移临时翻倍。
3. 重试仍留在任务原 transport；由旧任务产生的新事件按新策略进入 RabbitMQ。同一个消息不做双 Broker 广播。
4. GC 按原计划到期执行；无法等待时进入专门转移工具流程，不能手工 RunTask 使其提前。
5. 每次盘点核对 Asynq 活跃/待执行状态、Outbox 非 published、RabbitMQ ready/unacked/retry/DLQ、业务未完成状态和终态失败清单。
6. 若旧 Asynq 数量持续新增，追查未升级生产者或兼容补投，而不是继续等待排空。

### 4.4 结束兼容期

1. 旧任务需执行状态和旧 delivery 均归零，archive 有明确审计结论，观察窗通过。
2. 停止旧 Asynq 消费和 relay，再观察是否出现新的旧 transport 写入。
3. 发布退役变更：删 Asynq 适配、依赖、专用配置和探针；保留必要的历史任务解码/审计能力。
4. 对独立 Redis Broker 停用给出影响清单；默认不删除其卷。共享 Redis 仍用于 Cache/Coordination 时必须继续运行。

## 5. 回滚 Runbook

**回滚的安全目标是恢复可执行链路并保留所有未完成任务，不是让系统立即回到只装有旧 Asynq 二进制的状态。**

### 5.1 RabbitMQ 尚未接收业务任务

暂停 relay/生产者，将新发布策略恢复为 Asynq，核对是否存在 RabbitMQ pending/sending delivery。对证实从未发布的记录按审计流程改归属，再恢复兼容 Asynq 链路。schema 使用 expand 结构，不急于执行 down migration。

### 5.2 RabbitMQ 已确认业务任务

1. 恢复兼容版本，暂停 RabbitMQ 新发布，将新业务任务切回 Asynq。
2. 保留 RabbitMQ consumer 排空已确认消息及 retry queues；RabbitMQ 的 published 行不自动重发到 Asynq。
3. 若 Broker 不可用，先修复或恢复 Broker；只将能够由业务状态或记录明确确认未完成的任务按审计清单重建，不能仅凭 published 状态推断已完成或未完成。
4. 无法自然排空时，由受控 transfer 工具完成“目标 Broker 确认 -> 交接记录持久化 -> 原任务确认/解除归属”；崩溃窗口允许重复，要求消费幂等和断点续跑。
5. 搬运 scheduled/retry 必须保留 due time、已用 retry、业务 task_id、事件身份和版本。active 任务应先停止接收并完成或交回，不能在其仍执行时复制运行。
6. 两个 Broker 的未完成任务和业务中间态全部解释清楚后，才结束兼容运行。

数据库回退默认采用应用兼容回滚。新增字段/索引保留，避免删掉仍用于追踪新任务的状态；清理 schema 另做收缩变更。

## 6. 验收矩阵

以下为必须执行的验证项，不是本轮已通过的结果。每项记录消息 ID、关联业务记录、故障窗口、结果、持续时间和遗留数据。

| 类别 | 场景 | 必须观察到的结果 |
| --- | --- | --- |
| 事务 | 业务事务提交失败 | 不出现孤立的可执行任务 |
| 发布 | RabbitMQ 停止或拒收 | delivery 留在可重试状态，核心业务主库可继续提交 Outbox |
| 发布 | exchange 有效但没有匹配 binding | mandatory return 使发布失败，不能误标 published |
| 发布 | Broker 接受后 Relay 在 DB ACK 前被杀 | 稳定身份重投，业务结果无重复破坏 |
| 发布 | confirm 丢失、nack、超时、channel 关闭 | 未决结果可重试，不将未知结果当成功 |
| 租约 | Relay claim 被接管后旧进程返回 | 旧 lease_token 不能覆盖新发布状态 |
| 选主 | 双 transport Relay 和同 transport 多副本同时运行 | 两个后端持续投递，同后端选主与租约保护有效 |
| 消费 | handler 完成前进程被杀 | 未 ACK 消息重新投递 |
| 消费 | 业务提交后 ACK 前进程被杀 | 重投被业务幂等/消费记录正确吸收 |
| 重复 | 同 event/canonical destination 并发到达 | 文件、配额、引用数、AI 版本与索引结果一致 |
| 重试 | 首次失败、最后一次失败、MaxRetry=0 | 计数和退避正确，终态回调不遗漏 |
| 重试 | 下一次 retry 已确认但原 ACK 丢失 | 允许重复消息，业务可收敛，不产生不可控重试风暴 |
| DLX | retry 到期时目标 queue 不可用 | 消息保留/重试，可观测，无静默丢失 |
| DLQ | Permanent、坏 payload、未知 task type | 有持久记录、原始身份和处置路径，不无限回队 |
| DLQ | Broker delivery-limit 触发 | 可审计，所需业务终态或人工修复可完成 |
| 终态 | FailVerification/失败记录写入暂时失败 | 不提前 ACK，配额最终只释放一次 |
| GC | 新任务和历史 scheduled task | 不早于原 due time；重启/重试不重置等待窗口 |
| GC | 到期前对象被重新引用 | 不删除有效对象 |
| AI | 创建、重建、超时、永久错误 | 原 async_tasks ID 正确，状态收敛，版本 fencing 保持 |
| Search | 重投、乱序、删除后旧事件重放 | 不复活 tombstone，不覆盖更新版本 |
| Recovery | Broker 重复丢失、长执行、永久失败 | 可恢复丢失任务，不反复复活永久错误，不错误绕过 GC 延迟 |
| 生命周期 | SIGTERM、强杀、断线、consumer cancel | 停机有界，未完成消息可接管，重连后订阅恢复 |
| 超时 | 接近 30m 的任务、忽略 context 的测试 handler | ACK timeout 预算正确；不在旧执行仍运行时当作已完成 |
| 配置 | AI 关闭、搜索关闭/开启、合并和拆分进程 | 路由与 handler 覆盖一致；无错误队列争抢 |
| 兼容 | 旧任务名称、旧 payload、旧 delivery 别名 | 可解析执行，不生成双 transport 目标 |
| Kafka | 开关变化、Kafka 不可用 | MQ 迁移未破坏独立事件流和 delivery 状态 |
| 预算 | 双后端排空、AI 长任务、突发积压 | 并发/连接/内存受限，慢任务不饿死对象校验 |
| 回滚 | 已确认到 RabbitMQ 后回滚 | 消息有归属、到期和重试信息保留、业务最终收敛 |

单机可以验证上述应用窗口的大部分行为，但 RabbitMQ 多节点多数派丢失、跨主机网络分区和持久磁盘故障另行验证。故障测试不能对当前用户数据和共享 Broker 直接执行。

## 7. 验证命令框架与交付物

实施后首先运行修改范围对应的单元/契约测试，再在隔离依赖上跑集成测试。以下 RabbitMQ package 路径和环境变量属于计划新增项，当前仓库尚不能直接执行全套命令：

```bash
GOMAXPROCS=2 go test -p 1 -count=1 \
  ./internal/taskqueue/... ./internal/platform/rabbitmq/... \
  ./internal/outbox ./internal/platform/worker ./internal/bootstrap \
  ./internal/upload ./internal/media ./internal/objectstore \
  ./internal/ai ./internal/search ./internal/recovery
```

集成验证使用隔离的 `TEST_POSTGRES_DSN`、`TEST_REDIS_ADDR` 和拟新增 `TEST_RABBITMQ_URL`，详细故障 harness 在 P4 实现。需要并发验证的适配层和 Outbox 再运行有针对性的 race 测试；真实 Broker 测试必须列出实际通过和跳过项。

每阶段交付至少包含：

- 代码/配置 diff 以及接口与数据所有权说明。
- 迁移 SQL、执行影响、回滚边界和验证结果。
- 实际测试命令、PASS/SKIP/FAIL、负载参数和故障窗口证据。
- 镜像摘要、Broker/客户端版本、应用角色和运行配置摘要，不包含密钥。
- 旧队列盘点、交接/重放审计、未解决失败任务清单。
- “已实现、已测试、已部署、仍待完成”分别记录。

## 8. 当前交付状态

已完成：源码与配置评估、RabbitMQ 只读版本/队列盘点、当前代码 6 个 package 的针对性检查，以及本计划和配套改动说明。

未开始：RabbitMQ 代码接入、数据库迁移、应用部署、业务队列创建、任务搬运、故障演练和容量验证。

下一实施动作是 P0 契约冻结和历史任务盘点，随后先完成 P1 的兼容解耦与业务 task_id 回归，再接入 RabbitMQ。
