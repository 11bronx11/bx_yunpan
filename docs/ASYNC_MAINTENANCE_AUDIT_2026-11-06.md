# 异步执行、维护与 GC 竞争审计

日期：2026-11-06（北京时间；工作和测试始于 11-05）  
状态：代码修复与隔离集成验证完成；未部署，未迁移业务数据库。  
基线：[上传与维护职责拆分](UPLOAD_MAINTENANCE_SPLIT_PLAN_2026-11.md)。本轮保留已有拆分改动，没有重置工作区。

## 结果与执行边界

PostgreSQL 继续作为状态权威。定时维护扫描负责发现、收敛数据库状态和登记 Outbox；split upload worker 执行上传操作，maintenance worker 执行对象和衍生文件回收。combined worker 支持相同操作。

本轮修复了操作租约过期后的重复执行、无限创建恢复任务、批次被坏任务阻塞、上传失败后的配额和清理状态不收敛，以及缩略图写入与 GC 的交错窗口。测试证明下述受控竞争和故障窗口能够收敛；不代表任意网络分区、任意 S3 实现或跨主机 HA 均已得到验证。

## 发现的问题及修复

| 问题 / 触发窗口 | 当前处理 |
| --- | --- |
| MQ 执行 receipt 只保护一个 TaskID，新的恢复任务可使用不同 TaskID；慢存储调用超过操作租约后被再次接管 | provider 调用期间续租 durable storage operation；成功、失败提交仍检查操作 token，失去租约时取消调用上下文 |
| 旧 worker 的失败提交可能把替代 worker 的失败当作自己的提交成功 | 严格返回 lease lost；失败状态持久化错误向上返回 |
| worker 退出使 provider 上下文取消，随后失败状态也无法落库 | 使用脱离调用取消的 5 秒上下文持久化；正常 shutdown cancellation 释放操作所有权并退回本次尝试次数 |
| 单个 MQ 任务耗尽后，扫描器不断创建新事件，重置消息的重试预算 | durable operation 有总尝试上限，恢复事件家族另有持久化数量上限；原始 receipt 和失败记录保留 |
| handler 返回成功但没有推进业务状态，维护仍无限补任务 | 新恢复事件也计入家族预算；耗尽后写 `stopped_at` 并停止自动恢复 |
| 预先 claim 整批操作，第一项失败后直接返回；剩余项长期持有无执行者的租约 | 每次只 claim 马上执行的一项；失败不阻塞同批其他项 |
| 最老一批 active/dead/坏候选反复占满 LIMIT | 各类扫描使用独立 UUID 游标并回绕；扫描类别轮换、单项错误聚合后继续 |
| 最终失败时领域状态提交也失败，upload 留在 initializing/completing/verifying 并长期占配额 | 周期维护识别终止操作、停止恢复家族和 dead verification receipt；锁会话，释放一次配额，标失败并登记 abort/staging cleanup |
| database-only maintenance 调用 staging cleanup 时没有 provider 客户端 | 持久化清理和 Outbox 后结束，由 upload worker 执行 |
| multipart abort 重复调用返回 NoSuchUpload | 作为已完成的幂等清理处理 |
| verification 失败时 canonical copy 仍在返回，GC 先删后被 copy 重建 | GC 等待同 bucket/key 的 running canonical.ensure；新对象代次使用独立 key |
| 不支持的图片、过大图片被当作成功，缺失缩略图又不断触发恢复 | 返回终止任务错误；存储读取错误仍可重试，避免 image.DecodeConfig 把读取故障伪装成格式错误 |
| 缩略图已经写到 MinIO，GC 先完成，随后元数据提交失败，留下无记录文件 | 写 provider 前登记并持有 `variant.delete` 补偿操作；元数据与取消补偿在同一事务提交；被淘汰或中断的写由 maintenance 回收 |
| 两个缩略图任务写同一 key，一个失败后的清理误删成功者 | 每次写入使用唯一 key；清理只删除该次尝试的 key，并检查是否已经被 ready variant 采用 |
| 对象删除后复用 ID，旧缩略图停止记录或 dead receipt 阻断新代次 | 缩略图恢复预算绑定 canonical key；按该代次 verified_at 隔离旧事件。普通引用/version 变化不会重置预算 |

遗留 `running` execution 的租约为 NULL 时，超过恢复宽限也能重放。GC 恢复扫描跳过已有 pending/running/failed/dead object.delete 的对象，防止重复创建 GC 家族。

## 失败与停止策略

1. **存储操作最多 12 次尝试。** `storageops.MaxAttempts` 属于持久化 operation，新 MQ 事件和进程重启均不能重置。失败至少等待 1 秒、5 秒、30 秒、1 分钟，之后每次至少 5 分钟。活动租约不会因达到上限而被扫描器强行终止；最后一次执行若崩溃，租约过期后也能进入 dead。
2. **每个逻辑恢复家族最多 3 个恢复事件。** `recovery.MaxRecoveryEvents` 统计 `outbox_recoveries`，保留原事件和 receipt；包括成功却没有推进领域状态的恢复。单条事件仍受任务自身的 broker 重试预算约束。原始业务事件是否计入家族取决于它是否由该 recovery key 登记，因此 3 不是 provider 的总调用次数。
3. **两个上限任一先到即可停止工作。** storage recovery 家族耗尽时，未成功、无活动租约的 storage operation 转 dead；已经成功的操作保留 succeeded。需要收敛的上传会话由单独的数据库维护路径转失败并释放配额。abort、staging delete、variant delete、GC 各自保留清理记录和预算。
4. **确定不可处理的任务直接终止。** 无效任务 payload、已停止存储工作、不支持或超过像素限制的图片，返回 SkipRetry。格式损坏的其他图片仍可能走完有限重试预算。
5. **终止不等于抹掉工作。** 保留 operation、last_error、completed_at、recovery、task result 和失败记录，输出结构化 error 日志。增加 dead storage、stopped recovery 指标及 `YunpanAutomaticWorkStopped` 告警。耗尽后需要修复根因并显式重新发起或由运维修复，不能靠周期任务换身份无限重试。当前没有新增面向用户的一键修复 API。
6. **数据库故障不取消必要的状态收敛。** 周期性数据库扫描每次有 30 秒期限，记录故障后下轮继续；一个坏候选不会挡住后续候选。Outbox 在 broker 不可用时继续保留待发布工作。不能因数据库或 broker 暂时不可用就删除配额、上传或清理记录。

12 次和 3 个事件是本轮明确加入的保守代码常量。生产环境若需调整，应结合积压年龄、provider 故障恢复时间和人工响应策略修改并回归；它们不是容量测量所得参数。

## 缩略图与 GC 的具体顺序

注册缩略图写入时，短事务持有对象 SHARE 行锁，确认 ready 和 canonical key，再创建并 claim 补偿。GC 的 ready → deleting 需要 UPDATE 行锁，因此不会遗漏一个已经注册的写。provider 调用在事务之外执行，补偿操作持续续租。

写入完成后，事务锁补偿操作、检查 token，再检查对象代次和状态，尝试发布唯一 variant。发布成功与补偿 succeeded 一起提交；并发败者或对象已退休时，补偿留下待清理的唯一 key。失去 token 的旧写入者不能再发布元数据。

GC 看见 running variant 补偿时暂缓；补偿恢复只删除未被 ready variant 采用的 key。对象复活的 canonical key 与缩略图尝试 key 都隔离旧代次。

## 验证证据

测试使用本轮新建的独立 PostgreSQL、RabbitMQ、MinIO；每个测试包建立新数据库并运行 migration。没有读取业务环境密钥或连接现有业务数据库。服务端点仅绑定 loopback。

| 验证 | 结果 |
| --- | --- |
| 修复过程中的完整后端集成覆盖，真实 PostgreSQL/RabbitMQ/MinIO | 33 个含测试的 package：409 项测试/子测试通过、7 项跳过；后续新增竞争用例另外回归 |
| 初轮核心 8 包 `-race` | 148 通过、5 跳过 |
| 后续 storageops/recovery/upload/media/bootstrap/dbmetrics `-race` | 67 通过、0 跳过 |
| 加入缩略图补偿及独立维护副本后的 7 包 `-race` | 73 通过、0 跳过；包含真实 MinIO 的三种确定性交错 |
| 新代次恢复修复后 recovery/outbox/bootstrap `-race` | 80 通过、0 跳过；包含旧 dead/stopped 与新代次隔离、引用变化不重置预算 |
| 六队列稳定性测试，concurrency=8，30 秒 | 1,488 次 publish confirm，1,488 个唯一消费，0 重复 |
| migration 30 正反向与保护条件 | 30 clean → 29 clean → 30 clean；dead operation、variant.delete 记录、stopped recovery 分别触发回滚拒绝；失败事务保持原约束 |
| 最终全包无外部依赖运行 | 33 个测试包通过，247 项通过、149 项因未配置外部依赖等跳过；19 个包没有测试；进程 exit=0 |
| 静态检查与告警 | go vet、git diff --check、Prometheus promtool 告警规则测试通过 |

完整集成覆盖的 7 项跳过包括 1 项真实 Redis 故障回退测试、2 项真实 ES/Qdrant 测试，以及 4 个仅由父测试启动的 helper 入口。AI 测试使用 fixture/fake provider，不能推断真实 AI 服务的容量或可用性。另修正一处 AI publication 测试中硬编码的旧 parser profile，改为显式测试 profile，没有修改 AI 运行逻辑。

真实端到端链路覆盖：API service 建立上传 → upload queue 执行 multipart create/complete → object queue verify → staging delete → 文件删除 → maintenance GC；同时运行两个独立 maintenance service/scanner；另外覆盖 expiry → 配额释放 → multipart abort。覆盖 provider 调用失败、worker crash/restart、租约接管和旧 owner fencing、receipt 丢失/重放、dead receipt、恢复预算持久化、坏候选饥饿以及配额只释放一次。

RabbitMQ 重启测试额外等待 retry queue → DLX → object queue 的真实 canary 完成后才删除隔离 vhost。曾复现 RabbitMQ 4.1.8 在恢复中立即删除测试 vhost 导致 DLX supervisor 退出，修改的是测试 teardown 边界；没有把此事描述为应用修好了 broker 本身。

原始日志、每包 JSONL、迁移日志和测试运行器：`/tmp/bx-async-audit-jsg19bpf/`。原始工作区 diff 同目录 `baseline.diff`。该目录属于主机临时证据，不是随仓库分发的构建产物。

验证结束后，逐一核对 `bx.async-audit=bx-async-audit-jsg19bpf` 标签及卷的独占挂载，已移除本轮 3 个测试容器和各自的 3 个测试卷；业务容器和卷未操作。清理结果在同目录 `cleanup-result.json`，日志保留。

## 迁移与上线顺序

新增 migration **000030_async_retry_bounds**：扩展 storage state 为 dead，加入 variant.delete 类型，新增 recovery.stopped_at。不能直接把本轮 worker 部署到仅有 29 或更早 schema 的数据库。

安排受控迁移窗口：先暂停旧 media/maintenance/combined 消费者，避免旧版本接收无法识别的 variant.delete；执行 migration 30，部署本轮 relay、maintenance、upload、object/media 或 combined worker，然后恢复流量/消费。新增 upload 队列和旧上传恢复消息的兼容要求仍遵循拆分文档。此顺序是代码兼容要求，本轮没有演练整套镜像滚动上线或 split/combined 实际部署切换。

down migration 会拒绝保留的新操作类型、dead 和 stopped 数据。即使 variant.delete 已成功，其记录也保留审计语义，不能把向旧 schema 的无损回滚当作可直接执行的操作。不要为降版本而批量删除队列、receipt、recovery 或 operation；应先制定并验证数据处理方案。

## 运行状态与验证限制

- 本轮只修改工作区；没有构建/切换业务镜像、迁移业务数据库或重启业务服务。
- 隔离验证运行在同一主机；30 秒稳定性测试属于短程回归，不是长期 soak 或生产容量认证。
- 未验证真实 Redis、ES/Qdrant 故障恢复；未验证跨主机网络分区、PostgreSQL 主从切换、多节点 RabbitMQ/MinIO、跨机 HA 或备份恢复。
- S3 不接收应用自定义 fencing token。续租、上下文取消和代次 key 能阻止已验证的旧 owner 数据库提交/跨代次误删，但无法证明 provider 在客户端取消后任意延迟完成的写绝不留下字节。取消后仍在服务端执行的极端迟到写，以及旧版本没有操作记录的孤儿，需要 provider 侧约束或对象清单审计补充保障。
- 终止的清理任务仍可能占用存储空间，这是有日志、记录和告警的显式人工处理状态；本轮没有悄悄把它标成已经删除。
