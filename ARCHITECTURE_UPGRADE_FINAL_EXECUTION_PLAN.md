# BX YunPan 最终架构升级施工计划

> 本文件是历史总架构施工手册，不是本轮高并发专项计划。2026-09-10 的当前计划见 `docs/HIGH_CONCURRENCY_UPGRADE_PLAN.md`，对应测试清单见 `docs/HIGH_CONCURRENCY_TEST_CHECKLIST.md`。

## 2026-09-10 本轮核心升级

### 目标

先缩短在线请求路径，再让缓存和索引承接可重复的读压力。本轮不改变 PostgreSQL 的事实所有权，也不把配额、权限、最终上传状态放入缓存。

### 已写入代码的 P0

1. **上传创建异步化**：事务只写 `upload_sessions` 和 `storage_operations(multipart.create)`；HTTP 直接返回 `initializing` 会话，不调用 MinIO，不做轮询。
2. **上传完成异步化**：请求只校验分片数量、切换 `completing` 并写唯一的 `multipart.complete` 操作；worker 完成外部操作后切换 `verifying` 并写校验事件。
3. **取消和过期清理异步化**：请求或清理循环只释放配额、切换状态和写 abort 操作，multipart abort 由 storage operation worker 执行。
4. **Outbox relay 短事务化**：事务只做 delivery claim，提交后调用 Redis/Asynq，再用独立事务 ack；稳定 TaskID 支持崩溃重试。
5. **业务缓存和热点索引**：新增有界 LRU + Redis + singleflight 的 JSON 读缓存；API 首批缓存目录 children 和文件详情；新增上传、operation、delivery、GC、async task、文件名查询索引。

### 关键不变量

- PostgreSQL 仍是目录、文件、对象引用、配额、上传状态和权限的唯一事实源。
- 缓存只保存可重建读模型；权限查询仍带 `owner_id`，缓存 key 也必须带用户 ID。
- `quota`、`reference_count`、最终状态和最终权限不走普通缓存。
- 所有外部对象存储调用都在数据库事务之外；异步投递至少一次，重复执行由唯一键、租约 token 和稳定 TaskID 吸收。

### 后续顺序

1. staging 验证 `000022` 迁移、索引命中和 `sending` 租约回收。
2. 用真实 MinIO/Redis 跑上传创建、完成、取消的故障窗口测试。
3. 增加缓存命中率、回源率、错误率指标，再调 TTL 和 L1 容量。
4. 观察 PostgreSQL 连接池、Outbox backlog、storage operation backlog 后再扩大 worker 并发。
5. 搜索继续按 PostgreSQL 权限过滤；ES/Qdrant 只作为通过影子查询验证后的派生索引。

### 回滚

- 应用回滚前保留 `000022`；旧版本不依赖 `sending` 状态。
- 缓存可通过 `CACHE_ENABLED=false` 关闭，关闭后直接走 PostgreSQL。
- Outbox claim 只处理 `asynq` delivery；Kafka 路径不与本轮混用。

> 读者：Codex（执行者）
>
> 架构基线：`/home/bronx/remote/bx_yunpan/ARCHITECTURE_UPGRADE_FINAL.md`
>
> 适用仓库：`/home/bronx/remote/bx_yunpan`
>
> 本文是施工手册，不是产品宣传材料。每一个阶段都必须以源码、测试、运行探针、迁移记录或故障演练作为完成证据。

## 0. 使用规则

### 0.1 权威关系

1. `ARCHITECTURE_UPGRADE_FINAL.md` 是目标架构的权威设计文件。
2. 本文把目标架构转换成可执行任务、文件边界、迁移、配置、测试和回滚步骤。
3. 当前源码是实现事实。目标文档中尚未实现的部分必须标为“计划中”，不能写成“已完成”。
4. 仓库中其他两份 `ARCHITECTURE_UPGRADE*.md` 不参与本轮设计判断；旧的 `docs/bx_yunpan_升级执行计划书.md` 仅作为历史记录，不覆盖本计划。

### 0.2 Codex 每次施工的固定循环

每个工作包都按下面的顺序执行，不能只改代码后补说明：

```text
读当前源码和运行配置
  -> 写出本工作包的状态、不变量和失败窗口
  -> 先做兼容性/迁移设计
  -> 实现最小改动
  -> 添加针对性测试
  -> 执行本工作包的验证命令
  -> 保存证据（日志、指标、SQL、截图或测试输出）
  -> 更新本计划的完成状态
  -> 再进入下一个工作包
```

### 0.3 不可违反的施工纪律

- 不删除数据库、Docker volume、对象存储、缓存或用户数据。需要清理时先做只读盘点并等待明确授权。
- 不打印、写入日志或提交 `DASHSCOPE_API_KEY`。
- 迁移使用 expand/contract：先加字段或表，再兼容旧读写，完成回填和验证后才删除旧结构。
- 任何跨模块改动必须先确认数据所有权；不能因为“服务拆分”让模块直接写别人的表。
- 不把服务数量、容器数量、单机 p95 或 fake Provider 结果写成生产能力。
- 不把 Kafka 当成 Asynq 的替代品；不把 ES/Qdrant 当成权限事实源。
- 同一聚合内继续使用本地 PostgreSQL 事务；跨聚合或跨服务只用命令、事件和 Saga。
- 所有异步消费者按至少一次投递设计，必须能安全重复执行。
- 任何新配置都必须同时适配：`config.go`、`.env.example`、`.env.no-ai.example`（如适用）、`compose.yaml`、`config-check.sh` 和相关启动脚本。
- 默认开发环境保持可启动；新基础设施必须放在 profile 或显式 feature flag 后面。
- 每个提交只包含一个工作包的逻辑变化；提交前至少执行 `gofmt -w`、`go vet ./...`、`go test ./...` 和受影响的前端检查。

## 1. 已核实的当前基线

### 1.1 当前运行结构

```text
web（React + Nginx）
  -> api（Go Gin，当前同时承担 core-api 所有业务）
       -> PostgreSQL 17 + pgvector
       -> Redis（Asynq、锁、AI 限流共用）
       -> MinIO（单节点）
       -> aisvc（gRPC + AI Asynq worker，同一进程）
  -> worker（校验、缩略图、GC、Outbox dispatcher）
  -> etcd（aisvc 注册发现，当前 Compose 单节点）
```

启动入口：

| 进程 | 入口 | 当前职责 |
|---|---|---|
| API | `backend/cmd/api/main.go` | HTTP、鉴权、目录、上传控制面、分享、AI HTTP 适配 |
| Worker | `backend/cmd/worker/main.go` | Outbox dispatcher、上传过期清理、对象校验、缩略图、GC |
| aisvc | `backend/cmd/aisvc/main.go` | gRPC Search/Ask/Reprocess、AI `ai` 队列、Provider、限流、etcd 注册 |
| Migrate | `backend/cmd/migrate/main.go` | 版本化数据库迁移 |

API 的服务组装位于 `backend/internal/bootstrap/api.go`；Worker 组装位于 `backend/internal/bootstrap/worker.go`；AI 组装位于 `backend/internal/bootstrap/aisvc.go`。

### 1.2 当前核心事实和所有权

| 事实/数据 | 当前所有者 | 不变量 |
|---|---|---|
| 用户、Refresh Session | `identity` | Access Token 由统一签名配置生成；Refresh 轮换并检测复用 |
| 目录、逻辑文件 | `drive` | `owner_id` 隔离；active 名称唯一；版本乐观锁 |
| 物理对象 | `objectstore` | `sha256 + size` 唯一；FileEntry 通过引用计数使用对象 |
| 上传会话、分片 | `upload` | 幂等键唯一；配额预占/提交/释放只能发生一次 |
| 分享和导入 | `sharing` | Share Key 只保存 HMAC；导入用幂等键；不复制物理对象 |
| AI 文档、切片、向量 | `ai`/`aisvc` | 每个对象一份文档；Pipeline/Model 版本控制重建 |
| Outbox | `outbox` | 业务数据和事件同事务提交；重复投递由 Task ID 和消费者幂等吸收 |
| 异步任务状态 | `async_tasks` | 用户只能查询自己的任务；重试不重复写有效状态 |

当前数据库迁移为 `000001` 到 `000016`；`000008` 已有 `outbox_events.trace_context`，`000009` 到 `000014` 增加 delivery、聚合版本、storage operation、lease fencing 和 recovery 基础，`000015` 增加 AI 文档版本，`000016` 增加 owner 内容/ACL epoch。`event_consumptions` 表已存在，当前业务代码已提供统一消费幂等接口但仍需完成全量消费者收口。

### 1.3 当前真实能力与目标差距

已经具备：

- 模块化单体和独立 `aisvc`；
- API 无本地会话、对象内容不经过 Go API；
- PostgreSQL FTS + pgvector、并行召回、RRF、可选 Reranker、Citation 校验；
- Transactional Outbox + Asynq；
- etcd lease 注册、gRPC resolver、round_robin、熔断、超时和按接口重试；
- Request ID、OTel Trace、Prometheus 指标和健康探针；
- SHA-256 强校验、秒传、引用计数和异步 GC。

尚未实现，必须按本计划逐阶段推进：

- `core-api` 多副本的完整部署和摘流量验证；
- 独立 Outbox Relay，以及按资源类型独立扩容的 Worker 进程；
- `ai-query` 与 `ai-indexer` 的独立扩展单元；
- Redis Broker/Coordination/Cache 职责隔离和两级业务缓存；
- PostgreSQL 主备 HA、备份/WAL/PITR、故障转移演练；
- Elasticsearch、Qdrant、双写、影子查询和回退；
- Kafka 长期事件流、DLQ、重放和长期消费者；
- 条件式业务服务拆分；
- 物理分库、分片和生产级基础设施集群。

当前低内存开发机只用于功能和小规模验证。PostgreSQL HA、Redis HA、MinIO HA、Kafka 多节点等必须在专用 staging/生产环境演练，不能在当前单机上并行冷构建或把结果当容量结论。

## 2. 目标拓扑和中间态

### 2.1 最终拓扑

```text
Browser
  -> Gateway / Load Balancer
       -> core-api（无状态，多副本）
       -> ai-query（gRPC，多副本）
       -> workflow（初期可内嵌 core-api）

业务事务
  -> PostgreSQL 业务数据 + Outbox
       -> Outbox Relay
            -> Asynq：object-worker / media-worker / ai-indexer / maintenance-worker
            -> Kafka：search-indexer / audit / 其他长期消费者

core-api / workers / ai-query / ai-indexer
  -> PostgreSQL HA（事实源）
  -> Redis Broker / Coordination / Cache（职责隔离）
  -> MinIO HA 或外部对象存储

ai-indexer -> ES/Qdrant 写入派生索引
ai-query   -> ES/Qdrant 召回 + PostgreSQL 权威权限确认
```

### 2.2 必须经过的中间态

不能直接从当前形态跳到最终形态。每个中间态都可以独立运行、独立回滚：

| 状态 | 变化 | 允许的运行形态 |
|---|---|---|
| S0 | 当前模块化单体 + aisvc | 现有 Compose，作为回滚基线 |
| S1 | API 明确无状态，支持多副本；事件契约和消费幂等收口 | `api=N`，仍使用单 PostgreSQL/Redis/MinIO |
| S2 | Outbox Relay 与资源 Worker 分离 | Relay、object、media、maintenance 可独立扩容 |
| S3 | `ai-query` 与 `ai-indexer` 分离 | 在线 gRPC 和离线 AI 队列分别扩容 |
| S4 | Redis 三职责隔离 + L1/L2 Cache | 缓存故障不影响事实写入；Broker/锁不共用连接配置 |
| S5 | PostgreSQL 主备 HA | 应用通过 writer 入口；完成 failover、连接池重建和 PITR 演练 |
| S6 | ES/Qdrant 双写和影子查询 | PostgreSQL FTS/pgvector 仍是 fallback |
| S7 | Kafka 长期事件消费者 | Asynq 仍负责短期执行任务；Kafka 故障不阻塞核心写入 |
| S8 | 仅对有独立收益的领域做物理拆分 | 先协议稳定，再数据库拆分；可回退到 core-api |

## 3. 全局不变量和契约

### 3.1 业务事实不变量

1. PostgreSQL 是用户、目录、文件、对象引用、配额、上传状态和权限的事实源。
2. ES、Qdrant、缓存、Kafka 事件、Asynq 任务都可以丢失后重建，不能反向成为业务事实源。
3. active `file_entries` 只能指向 `ready` `file_objects`。
4. 同一用户、同一对象只能有一个 active FileEntry；同一目录下名称大小写归一化后唯一。
5. 删除 FileEntry 必须在同一个本地事务内减少逻辑用量和对象引用；引用归零只写 GC 事件，不在请求内删除物理对象。
6. 配额余额、上传最终状态、对象引用计数和最终权限判断不走普通缓存。
7. 用户权限必须在 PostgreSQL 查询阶段过滤；派生索引上的 `owner_id`、`folder_id`、`acl_version` 只是候选过滤，不能替代权威确认。

### 3.2 异步不变量

```text
本地业务事务
  = 业务数据变更 + Outbox 事件 + 必要的幂等记录

Relay 投递失败
  -> Outbox 继续 pending

重复投递/重复消费
  -> Task ID、唯一键、聚合版本或 event_consumptions 吸收

Broker 已确认任务随后丢失
  -> 根据 PostgreSQL 未完成业务状态重新生成确定性任务

派生消费者失败
  -> 重试/DLQ/回放，不回滚已经提交的核心业务事实
```

领域事件和执行任务必须分开命名：

- 领域事件：描述事实已经发生，例如 `file.created.v1`、`file.updated.v1`、`file.deleted.v1`、`acl.changed.v1`。
- 执行任务：描述某个消费者要做的工作，例如 `object.verify.v1`、`media.thumbnail.v1`、`ai.index.v1`。

统一事件信封至少包含：

```json
{
  "event_id": "uuid",
  "event_type": "file.updated.v1",
  "aggregate_type": "file",
  "aggregate_id": "uuid",
  "aggregate_version": 4,
  "producer": "core-api",
  "occurred_at": "2026-09-04T00:00:00Z",
  "trace": {"traceparent": "...", "tracestate": "..."},
  "payload": {}
}
```

事件消费者的标准顺序：

```text
解析信封
  -> 校验 event_type 和版本
  -> 校验聚合版本是否过旧/乱序
  -> 执行幂等副作用
  -> 与副作用同事务写 event_consumptions（若副作用在 PostgreSQL）
  -> ACK
```

外部索引的消费记录必须在“索引文档版本已经幂等写入”之后记录，不能先标记 consumed 再写 ES/Qdrant。

### 3.3 配置命名原则

- 保留已有配置名，新增配置使用明确的职责前缀，不把同一环境变量静默改义。
- 写入口和读副本分开命名，例如 `POSTGRES_PRIMARY_DSN`、`POSTGRES_REPLICA_DSNS`；没有完成粘滞读验证前，所有业务读仍走 primary。
- Redis 使用 `REDIS_BROKER_*`、`REDIS_COORDINATION_*`、`REDIS_CACHE_*`；兼容期可由旧 `REDIS_ADDR` 派生三者，但启动日志必须标出兼容模式。
- 搜索读取使用 `SEARCH_READ_BACKEND=postgres|shadow|es_qdrant`，默认 `postgres`。
- Kafka、ES、Qdrant、HA 基础设施默认关闭或放在独立 Compose profile，不能改变普通开发启动。

## 4. 分阶段施工包

下面的任务编号是执行顺序。每个施工包完成后才能进入下一个依赖它的包。

状态标记统一使用 `[ ] 未开始`、`[~] 进行中`、`[x] 已验证`、`[!] 阻塞`；当前状态以本节“本轮施工状态”和各工作包的最新记录为准。“方案已写入本文”不等于代码已完成。每个 `P<n>.<m>` 是验收单元，不是强制单个提交：若同时包含 migration、兼容代码和 contract 清理，必须拆成可独立部署的多个 PR，并在本节标题后记录 PR/commit、验证证据路径和回滚开关。不得把跨两个阶段的实现塞进同一提交。

### 本轮施工状态（2026-09-04）

- P0.1 `[x] 已验证`：基线证据见 `artifacts/architecture-upgrade/20260904T123433Z/`。
- P0.2 `[~] 进行中`：000009 expand migration、typed event contract、`event_consumptions` 消费接口、显式 delivery routing 和兼容 relay 已落地；最终测试、真实 PostgreSQL 集成测试和 Compose 状态证据见 `artifacts/architecture-upgrade/20260904T150559Z/`。尚未完成旧 Relay 兼容窗口故障演练、V2 flag 行为演练、unrouted 指标和 contract migration，不能进入 P0.3。
- P0.3-P4 `[~] 进行中`：源码和单机 profile 已有实现，但仍缺完整兼容窗口、故障矩阵和专用环境验收。
- P5.2 `[x] 已验证（代码/单机）`：`POSTGRES_PRIMARY_DSN` writer 兼容适配、迁移入口和配置门禁已落地；HA failover/PITR 仍待 staging。
- P6.1-P6.2 `[x] 已验证（代码/单机）`：版本化 search projection schema、PostgreSQL fallback adapter、独立 `search-indexer` 和 feature flag 已落地；ES/Qdrant snapshot/shadow/cutover 仍待 staging。
- P7.1 `[x] 已验证（接口/单机）`：Kafka producer boundary 和校验测试已落地；真实 Kafka broker、DLQ、watermark replay 仍待 staging。
- P8.5 `[x] 已验证（迁移）`：workflow/Saga 持久模型已扩展到 `000019`，并补齐持久状态迁移、失败与补偿 API；远程服务故障注入和唯一 writer canary 仍待 staging。
- P9 `[~] 受环境门禁`：已提供 `deploy/staging-ha-check.sh` 与 `deploy/staging-search-check.sh` 前置检查；生产 HA、Gateway、物理分库不能在当前单机完成。

### 2026-09-05 P6-P9 继续施工记录（08:25 UTC 后）

- P6：搜索投影新增 `IndexVersion` 生命周期（building/active/failed/retired）、独立 content snapshot checkpoint、ACL epoch 投影和删除 tombstone 版本保护；迁移推进到 `000019`，真实 PostgreSQL migration 为 `19|false`。
- P7：新增 Kafka-only `KafkaDispatcher`，只处理 `outbox_deliveries.transport=kafka`；发布 envelope 后才标记 delivery，失败保留退避状态，不触碰 Asynq delivery。Kafka route 仅接收 domain event，command 仍留在 Asynq。
- P8：workflow service 新增 command 成功/失败、实例/步骤状态迁移和补偿收敛，终态不可重开；`workflow_steps.updated_at` 纳入 `000019`。
- 新增 `deploy/staging-p9-check.sh`，以非破坏方式检查 P9 所需 HA、备份、对象存储、ES/Qdrant、Kafka、Gateway 和物理分库 endpoint；当前环境预期退出码 `2`，这是缺少专用 staging 参数的环境门禁证据。
- 新增 `deploy/P9_STAGING_RUNBOOK.md`，固定备份、恢复、唯一 writer/fencing、Broker 全丢重建、对象 checksum、索引 epoch/shadow、Kafka DLQ/replay 和 Gateway 回滚顺序；未通过专用 staging 门禁不得执行切换。
- 本轮代码验证：`go test ./...` 在干净环境通过（含 `TEST_POSTGRES_DSN` 的 PostgreSQL focused tests），`go vet ./...`、`bash -n deploy/*.sh`、`git diff --check` 通过。Kafka broker 故障/DLQ/watermark、ES/Qdrant shadow/cutover、远程 Saga canary、HA failover/PITR 和物理分库仍不能在当前单机宣称完成。

### 2026-09-05 继续施工记录

- 修复 recovery 扫描条件：pending/failed 必须满足 `available_at <= now`；无 lease 的 running operation 使用 `operationStaleAfter`，仍持有未来 lease 的 operation 不会被误回收。
- 移除 Upload Complete 第二事务中未 fencing 的 `MarkSucceeded`；Recovery 使用数据库 `ON CONFLICT DO NOTHING` 吸收并发扫描；`object.ready` 和 `object.gc_requested` 使用引用计数变更后的 committed object version。
- 修正 `status.sh`/`config-check.sh` 在查询 distributed profile 时遗漏 `app` profile 的 Compose 依赖问题。
- 证据见 `artifacts/architecture-upgrade/20260905T022640Z-p0-risk-fixes/`：Go 108/108、`go vet`、前端 14 suite/36 tests、受影响领域 PostgreSQL 48 tests、gofmt、shell syntax 和 diff check 通过。
- P0.2 仍为 `[~]`，P0.3-P2 仍未验收：当前 Compose 仍是旧单体容器，registry TLS 超时导致独立 Worker 镜像和双 API 重启未完成。

### 2026-09-05 本轮继续施工记录

- 修复 AI 不支持内容的终态收敛：`ai_document_versions` 与兼容 `ai_documents` 汇总行在同一事务更新；首代 unsupported 不再永久停留在 `processing`，已有 active version 时仍保留 `indexed` 查询入口。
- API 的新 HTTP Reprocess 路径改由 core-api 本地 command service 完成 owner 校验、幂等 Task 和 Outbox 写入；Search/Ask 继续经 query gRPC，旧 gRPC Reprocess 仅保留兼容入口。
- 新增 `000016_owner_content_epochs` 和 `internal/ownerepoch`：用户初始化、目录/文件内容变化、分享 ACL 变化在同一事务递增 epoch，事件 payload 携带提交后的 epoch；新增 PostgreSQL 回滚测试。
- `ai-query`/`ai-indexer` 独立角色补齐 metrics 端口；`up.sh --ai-split`、`status.sh`、`config-check.sh` 支持 split profile。当前通过宿主编译二进制完成 query health、indexer ai 队列与 metrics 探针。
- 迁移证据：隔离空库 `000001..000016` up 为 `16|false`，down-one 后为 `15|false`；带 legacy `processing` AI 文档的已有库升级后回填为 `building`，最终为 `16|false`。证据见 `artifacts/architecture-upgrade/20260905T-final-verification/`。
- 真实验证：后端 `go test ./...` 113 项通过、`go vet ./...` 通过；串行 PostgreSQL focused tests 85 项通过；前端 14 suites/36 tests 通过；双 API 已运行并在停止一个副本时保持 Web readiness 200，两个 instance ID 均出现。
- 当前状态仍不能写成全部完成：P0.2/P0.4/P1/P2/P3/P4 仍缺部分故障/容器验收；独立 Worker 镜像和 registry TLS 受阻，P5-P9 尚未施工。代码、宿主进程和单机 Compose 证据不等同生产 HA、ES/Qdrant、Kafka 或容量结论。

### 2026-09-05 继续施工记录（05:00 UTC 后）

- 修复 `deploy/config-check.sh` 查询 `ai-query` 时缺少 `--profile ai` 的 Compose 依赖错误；`bash -n deploy/*.sh`、完整 profile 渲染和 `git diff --check` 通过。
- 现有 `api`、`worker`、`aisvc` 按当前 Compose 环境重建（不拉取缺失镜像），`./deploy/status.sh` 通过；双 API readiness、Web/API/Storage 探针和配置漂移检查均为 OK。
- P1 摘除演练：停止 `api-1` 后 Web `/readyz` 连续 10 次 HTTP 200，恢复后两个副本均 healthy；证据见 `artifacts/architecture-upgrade/20260905T045336Z-api-replica-recheck/`。
- 四个独立 Worker role 已用宿主 Go 二进制分别启动；relay/object/media/maintenance 的 metrics endpoint 和队列所有权启动日志通过。补充统一 worker task/queue metrics（含 Asynq Inspector、失败/重试/归档、延迟和任务耗时），代码与测试见 `backend/internal/platform/worker/metrics.go`、`metrics_test.go`；宿主探针见 `artifacts/architecture-upgrade/20260905T050456Z-role-metrics-recheck/`。
- P3 split 宿主探针通过：`ai-query` gRPC health `SERVING`、`ai-indexer` 只消费 `ai` 队列且暴露队列指标；证据见 `artifacts/architecture-upgrade/20260905T050857Z-ai-split-recheck/`。历史队列中指向测试 bucket 的失败任务仍需隔离数据清理/回放，不据此宣称索引完成。
- 质量门禁：后端 `go test ./...` 115 项通过、`go vet ./...` 通过；真实 PostgreSQL focused tests 98 项通过；前端 14 suites/36 tests、lint、production build 通过。Docker Hub TLS 超时仍阻断 `migrate`、`minio-init` 和四个独立 Worker 镜像构建，因此 P1/P2 容器级新版验收仍为进行中。

### 2026-09-05 最终收口记录

- 部署脚本已完成双向模式切换修复：distributed/combined Worker 与 split/combined AI 不会同时消费同一队列；`--scale` 参数和显式服务顺序已修正，`etcd` 纳入 `ai-split` profile。
- P1/P2/P3 单机容器拓扑已实际启动：2 个 `api`、2 个 `ai-query`、1 个 `ai-indexer`、`outbox-relay`/`object-worker`/`media-worker`/`maintenance-worker` 各 1 个；combined `worker` 和 combined `aisvc` 已移除。`status.sh` 的逐副本 readiness、gRPC health、AI 队列角色、四个 Worker metrics、Web/API/Storage 和配置漂移检查均通过。
- P4 增加可选 `--redis-split`，运行态已按 Broker/Coordination/Cache 启动三套独立 Redis；应用配置支持 `CACHE_ENABLED`、`CACHE_TTL`、`CACHE_L1_MAX_ITEMS`。Cache Redis 不可用在 readiness 中报告 advisory/degraded，不阻断核心 API；PostgreSQL 和对象存储仍是硬依赖。
- Outbox 增加“Redis 已接受任务但 delivery 状态提交失败”的恢复测试：重启后使用稳定 Task ID 吸收重复入队。该证据只覆盖这一故障窗口，不等同于 Broker 全丢、全部业务中间态重建和多 Relay 故障矩阵已完成。
- 最终源码门禁见 `artifacts/architecture-upgrade/20260905T-final-closeout/`：Go 118 项/41 packages、`go vet`、gofmt、shell syntax、Compose 全 profile 渲染、配置检查、前端 14 suites/36 tests、直接 ESLint 和 production build 通过；运行态 `status.sh` 通过。`npm run lint` 被本机命令包装层错误扩展到 `web/build`，按 7.1 允许的本地 ESLint 等价命令复核通过，原始失败与替代结果均已保存。
- 现有后端应用镜像生成于 05:18 UTC，最后源码修改晚于镜像。最终重建先后受 Docker Hub TLS timeout、旧镜像代理 401 和备用代理下载停滞影响，已按用户要求停止；没有改仓库默认 registry，也没有停止或清理当前运行数据。当前容器证据证明部署拓扑和已有镜像可运行，不证明 05:18 UTC 后的最后源码已经进入镜像。
- 状态仍保持 P0.2/P0.3/P0.4/P1/P2/P3/P4 `[~]`：已具备大部分源码与单机隔离证据，但各包计划中的完整兼容窗口、外部故障矩阵或端到端业务剧本尚未全部满足。P5-P9 `[ ]`，PostgreSQL/Redis/MinIO HA、ES/Qdrant、Kafka、Saga、物理分库与生产 Gateway 必须在 staging/生产环境继续施工和演练。

### 2026-09-05 依赖和镜像刷新记录（06:43 UTC 后）

- Go 模块依赖通过临时 `GOPROXY=https://goproxy.cn,direct` 下载成功；Web 依赖通过临时 `https://registry.npmmirror.com` 执行 `npm ci` 成功，`npm ls --depth=0` 无问题。仓库默认依赖源未修改。
- 官方 Docker Hub 仍 TLS 超时，公开镜像站探测为 401 或超时；未继续外部基础层 pull。使用本机已验证的 Alpine/Nginx 运行时层，把当前工作树串行编译的 7 个 Go 目标和 React build 重新封装为本地镜像。
- `./deploy/up.sh --no-build --distributed-workers --ai-split --redis-split --api-replicas 2 --aisvc-replicas 2` 成功；`./deploy/status.sh`、`./deploy/config-check.sh` 通过。2 个 API、2 个 AI Query、1 个 AI Indexer、4 个独立 Worker、三套 Redis、PostgreSQL、MinIO、etcd 和 Web 均健康。
- 容器内 `/app/service` hash 与主机构建目标逐一匹配；迁移为 `16|false`，migrate exit 0。Go 118 项/41 packages、`go vet`、前端 14 suites/36 tests、直接 ESLint、production build、Compose render、shell syntax 和 diff check 均通过。证据见 `artifacts/architecture-upgrade/20260905T064353Z-dependency-image-refresh/`。
- 本记录只证明单机开发环境的依赖、镜像和 S1-S4 运行复验；P0.2-P4 仍缺计划中的完整故障矩阵/业务剧本，P5-P9 仍未开始，不能据此宣称生产 HA 或容量完成。

### 2026-09-05 接手续施工复验（10:15 UTC）

- 证据目录：`artifacts/architecture-upgrade/20260905T1015Z-handoff-continuation/`。
- 当前运行 PostgreSQL 的真实 DSN 下，`go test ./... -count=1` 172 项通过，`go vet ./...`、全量 Go 格式、`git diff --check`、`bash -n deploy/*.sh` 和 app/distributed/ai/search/redis-split Compose 渲染通过。
- 前端 14 suites/36 tests、直接 ESLint、生产构建通过；`status.sh` 和 `config-check.sh` 通过，迁移 `19|false`，Outbox pending/failed `0/0`。
- 配置默认值测试已显式隔离宿主环境中的可选 reranker 变量；配置断言移除完整结构体输出，避免失败日志泄露 DSN、Redis 密码或 AI provider key。
- 三组 staging 前置门禁在当前机器仍按预期返回 `2`，缺少专用 HA/PITR、ES/Qdrant、Kafka、Gateway 和物理分库 endpoint；不改变 P5-P9 的环境门禁状态。

### 2026-09-05 单机核心热路径闭环

- 证据：`artifacts/architecture-upgrade/20260905T1015Z-handoff-continuation/core-hotpath-summary.md`。
- 真实 HTTP 链路通过注册/登录、目录创建、预签名 Multipart、MinIO 写入、异步完成、下载字节校验、分享公开访问/撤销、文件/目录删除和退出登录。
- 重复 `Idempotency-Key` 返回同一 upload session；摘除一个 API 副本后 `/readyz` 连续 10 次 HTTP 200，恢复后 `status.sh` 通过。
- 停止 Cache Redis 时 `/readyz` 仍为 `status=ok`、缓存仅报告 `degraded`，登录和根目录读取 HTTP 200；恢复后容器 healthy。
- 当前事实库检查为迁移 `19|false`，Outbox event/delivery pending/failed `0/0`，storage operation 非终态 `0`，活动上传 `0`。
- 该记录关闭“核心热路径单机闭环”门槛，但不改变生产 HA、容量、RPO/RTO 或 staging 门槛；本轮未调用外部 DashScope provider。

### 2026-09-05 本地 staging 核心故障闭环

- 新增 `deploy/local-staging-check.sh`，以可逆方式演练 API 副本、Cache Redis、Broker Redis + Outbox Relay 和 MinIO 的停止、恢复与状态收敛；脚本不删除现有数据卷。
- Broker 窗口证明业务事实仍提交到 PostgreSQL，Outbox 在 broker 恢复后由 Relay 继续发布；Kafka producer 失败后保留 failed delivery、恢复后沿用同一 delivery 发布的测试已加入 outbox 包。
- Go 全量测试在脚本创建的临时 PostgreSQL 库上通过，迁移为 `19|false`；主库恢复后 `outbox_pending=0`、`outbox_delivery_pending=0`、`storage_non_terminal=0`，全部 Compose 服务 healthy。
- 产物：`artifacts/architecture-upgrade/20260905T141016Z-local-staging/`。该项完成本地核心逻辑模拟，不把单机结果提升为多机 HA、容量或生产 RPO/RTO 结论。

---

## P0 · 基线收口和契约准备

### 目标

把当前可运行系统收口为后续扩展的稳定基线，先补足事件、幂等、指标和验证工具，不引入新的外部基础设施。

### P0.1 记录当前事实和回滚点

涉及：

- `backend/internal/bootstrap/*.go`
- `backend/internal/platform/config/config.go`
- `deploy/.env`
- `deploy/compose.yaml`
- `deploy/status.sh`
- `docs/` 证据目录

动作：

1. 保存当前 `git status --short --branch`、镜像 tag、Compose 服务列表、迁移版本和健康探针结果。
2. 记录当前 `api`、`worker`、`aisvc` 的配置摘要，但不得输出密钥。
3. 记录数据库表数量、Outbox pending 数、Asynq 队列深度、AI 文档状态分布和对象引用计数分布。
4. 为每次后续演练创建带时间戳的证据目录，例如 `artifacts/architecture-upgrade/<timestamp>/`。

完成证据：

- `./deploy/status.sh` 成功；
- `go test ./...`、`go vet ./...`、前端测试成功；
- 证据目录只含脱敏结果。

回滚：不涉及数据修改；删除证据目录前需确认其不属于用户数据。

### P0.2 统一事件模型和 Outbox 投递状态

当前问题：`outbox_events.status` 只有一个状态，无法分别表达 Asynq/Kafka 以及同一 Asynq 事件的多个投递目标；当前 `event_version` 实际表示 payload schema 版本，事件模型还缺少 `message_kind`、`producer`、`occurred_at` 和明确的 `aggregate_version`。`outbox.Add` 每次生成新的 UUID，本身不是幂等接口，业务命令的唯一键和状态机才负责阻止重复事实事件。

新增迁移，建议文件：

```text
backend/migrations/000009_outbox_delivery.up.sql
backend/migrations/000009_outbox_delivery.down.sql
```

设计：

1. 保留 `event_version` 并明确其含义为 payload schema version，不再新增一个重复的 `schema_version`；新增 `message_kind`（`domain_event`/`command`）、`aggregate_version`、`producer`、`occurred_at`，保留 `created_at` 和 `trace_context`。
2. 新建 `outbox_deliveries`，状态落到具体 transport 和 destination，避免 `object.ready` 扇出 media/AI 时互相覆盖：

```text
event_id       uuid references outbox_events(id) on delete cascade
transport      varchar          -- asynq / kafka
destination    varchar          -- task type + queue，或 topic
status         varchar          -- pending / published / failed
attempt        integer not null default 0
available_at   timestamptz not null
published_at   timestamptz
last_error     text
primary key (event_id, transport, destination)
```

3. 在代码中维护显式 routing registry。`command` 只产生 Asynq delivery；`domain_event` 只产生已经启用且确有消费者的 delivery；`object.ready.v1` 是领域事实，同时派生 `media.thumbnail.v1` 和 `ai.index.v1` 两个 Asynq 目标。禁止未知事件默认落到 maintenance 队列。
4. Expand/contract 顺序必须固定：
   - 000009 先加 nullable 新列、delivery 表、CHECK/唯一约束和 `(transport, status, available_at, event_id)` 轮询索引，旧 `status` 列保留；
   - 回填 legacy 行时，`occurred_at=created_at`、`producer=legacy-core`，未知 `aggregate_version` 保持 NULL，不能用实体的当前版本伪造历史版本；
   - legacy `published` 行对应 delivery 直接标 published，`pending/failed` 行按现有 `fanout` 规则生成 pending delivery，绝不把已发布事件重新入队；
   - 部署兼容 writer/relay，过渡期同时维护旧状态和 delivery；验证无旧进程后，另开后续 migration 才允许收缩旧 `status/attempt/available_at/published_at`；
   - Kafka 在 P7 以明确 cutover watermark 创建 delivery；需要历史数据时先做事实库 snapshot，再从 watermark 接续，不能悄悄把全部旧事件重放到生产消费者。
5. `outbox.Add` 改为接收完整 envelope；routing policy 由 Outbox 包内的显式 registry 决定，领域调用方不能直接指定 Kafka topic 或 Asynq queue。一次 event row 对每个目标最多生成一个 delivery。重复业务请求是否产生新 event，由 Upload/Import/文件版本等业务幂等约束决定，不让 Outbox 猜测业务重复。
6. `event_consumptions` 现有 `(event_id, consumer_name)` 主键足以去重；先提供 `ConsumeOnce` 事务接口。只有出现按 consumer/time 清理或查询需求才加二级索引，且不对 `event_id` 增加 Outbox 外键，因为 Kafka 事件保留期可能长于本地 Outbox。
7. 事件 payload 与 trace 载荷分离：业务 payload 保持稳定，`trace_context` 只作为传输元数据，不让消费者业务 struct 依赖 `_trace` 字段。
8. 新增 `backend/internal/eventcontract/`，每个 v1 事件和命令使用具名 Go struct，禁止继续用散落的 `map[string]any` 作为稳定契约。目录至少包含 envelope、event type registry、typed payload、旧 payload fixture 和 JSON golden compatibility test；新增字段只能 optional，破坏性修改必须发布 `.v2`。
9. Outbox event 本身不可变。P7 之前没有 transport 的领域事件计入 `unrouted_events_total/oldest_age`，不能假装 published；P7 完成 snapshot + watermark 接续前不得清理。P7 后只有所有 delivery 都 published 且超过明确 retention 的事件才能归档/清理，failed、pending、unrouted 永不自动删除。

滚动发布顺序：

1. 先执行 expand migration，旧二进制继续只读写旧列。
2. 部署兼容 Relay：优先认 delivery，没有 delivery 时处理 legacy status；Task ID 算法保持 `event_id + destination` 稳定。
3. 部署双写新 API/Worker，新 writer 同时维护 legacy status 和 delivery；此时不得启用新事件类型。
4. 确认所有旧进程退出、旧队列排空后，打开 `OUTBOX_V2_EVENTS_ENABLED`，才允许 P0.3 的新事件进入系统。
5. P2 完成并验证回滚使用的是支持 delivery 的新版 combined worker 后，执行 contract migration 删除旧投递状态列；任何时刻都不允许旧 Relay 与只写新模型的 writer 混跑。

源码适配：

- `backend/internal/outbox/outbox.go`：事件信封、版本和 delivery 创建；
- `backend/internal/outbox/dispatcher.go`：先按 delivery 目标投递，并删除未知类型默认进入 maintenance 的行为；
- `backend/internal/eventcontract/*.go`：具名 payload、版本 registry 和兼容 fixture；
- `backend/internal/bootstrap/worker.go`：暂时保留兼容启动，但不再直接假设一个全局 status；
- `backend/internal/platform/worker/tracing.go`：从信封 metadata 提取 trace。

测试：

- 同一业务事务回滚时，事件和 delivery 都不存在；
- 同一 event row 对同一目标只能生成一条 delivery；重复业务命令由业务幂等约束阻止重复 event；
- 已发布 Asynq 事件迁移回填不重复入队；
- 同一 `object.ready` 的 media/AI 状态互不覆盖，Asynq/Kafka 状态也互不覆盖；
- legacy 未知聚合版本不会被伪造，未知路由进入可观测错误而不是 maintenance；
- 新 writer 与旧 Relay 的兼容窗口、V2 feature flag 和新版 combined worker 回滚各演练一次；
- v1 fixture 可以被新代码读取，v1 struct 的 JSON golden 未发生破坏性变化；
- pending/failed/unrouted event 不会被 retention job 删除；
- trace context 缺失时业务投递仍成功。

退出门槛：空库迁移成功、已有数据库升级成功、旧 API/Worker 可以继续处理旧事件、`go test ./...` 全绿。

回滚：只允许 down migration 用于空库或明确演练库；已有数据环境回滚使用前向兼容代码，不直接删除新增列/表。

### P0.3 补充聚合版本和领域事件

当前已经有 `file.created.v1`（仅秒传路径）、`object.ready.v1`、上传校验命令和分享导入事件等少量消息。为未来缓存失效和搜索索引补充领域事件，但保持第一阶段只写 Outbox，不立即引入 Kafka；没有已启用消费者的领域事件暂不创建 delivery：

| 事件 | 生产位置 | 聚合版本 |
|---|---|---|
| `file.created.v1` | 保留秒传路径，并补到 Verify 成功、分享导入 | FileEntry.Version |
| `file.updated.v1` | 文件改名、移动 | FileEntry.Version |
| `file.deleted.v1` | 文件软删除 | FileEntry.Version |
| `folder.created/renamed/moved/deleted.v1` | 对应目录事务 | Folder.Version |
| `share.created/revoked.v1` | 对应分享事务 | Share.Version |
| `object.ready.v1` | Verify 成功 | 新增并递增 FileObject.Version |
| `object.gc_requested.v1` | 引用归零；这是 command | FileObject.Version，Handler 仍重新检查引用 |
| `ai.reprocess_requested.v1` | 用户请求重建；这是 command | AsyncTask.Version 或首次请求固定为 1 |

`file_objects` 和需要多次状态迁移的 `async_tasks` 若没有稳定单调版本，必须在 expand migration 中补 `version bigint NOT NULL DEFAULT 1 CHECK (version > 0)`，状态/引用更新时原子递增；不能用 `verified_at`、`attempt` 或读取时的当前版本冒充事件发生时的聚合版本。Share 自然到期不是一次数据库写事务：到期正确性直接依据 `expires_at`，缓存 TTL 不得越过该时间；只有以后增加显式过期扫描状态迁移时才产生 expired 事件。

新增迁移：

```text
backend/migrations/000010_aggregate_versions.up.sql
backend/migrations/000010_aggregate_versions.down.sql
```

源码适配：

- `backend/internal/drive/service.go`：Create/Rename/Move/Delete、RenameFile/MoveFile 改为接收 context，并在同一 transaction 中完成状态更新、取得更新后的 version、写 typed event；
- `backend/internal/drive/files.go`：File Delete 无论对象是否归零都写 `file.deleted.v1`；只有引用归零才额外写 `object.gc_requested.v1`；
- `backend/internal/upload/service.go`：秒传、Verify 成功都写 `file.created.v1`，Verify 同时写 `object.ready.v1`；
- `backend/internal/sharing/service.go`：Create/Revoke 与事件同事务；Import 写 `share.imported.v1` 和新 FileEntry 对应的 `file.created.v1`；
- `backend/internal/objectstore/service.go`：引用、ready/deleting/deleted 状态变化原子递增 Object.Version；
- `backend/internal/ai/service.go`：AsyncTask 状态变化原子递增 Task.Version，reprocess command 使用创建时版本。

payload 最小字段：`owner_id`、聚合 ID/版本、关联 `object_id`；rename/move 同时携带 old/new name 或 parent/folder；delete 携带 tombstone time；不得包含 Share Key、Token、对象内容或 Provider 密钥。事件必须在 service transaction 内生成，HTTP handler 不生产领域事件。

要求：

- 事件必须和对应写事务同提交；
- 事件生产失败必须让业务事务失败；
- 同一次状态变化不要在多个层重复写事件；
- 不把“查询结果”当事件；
- 旧事件类型保留兼容解析，新增版本只向前兼容。

测试：

- 每条写路径验证“业务状态和事件同时提交或同时回滚”，不能只断言 event count；
- stale `If-Match`、名称冲突、配额冲突和重复 Import 不产生额外事件；
- 普通 File Delete 产生 file tombstone，最后一个引用删除才产生 GC command；
- 同一共享 object 被不同用户引用时，删除一方只影响该 FileEntry；
- old/new 字段与 committed aggregate version 精确一致。

退出门槛：全部状态变更入口已盘点且无绕过事务的写路径；typed contract、事务回滚、并发幂等和旧事件兼容测试通过；V2 flag 关闭可停止新事件生产而不影响核心写入。

回滚：关闭 `OUTBOX_V2_EVENTS_ENABLED` 停止新事件类型；保留已经提交的事件和 version 列，不用 down migration 删除生产数据。

### P0.4 明确外部副作用边界

当前 `upload` 已有可注入的 `multipartStorage` 和 `uploadStorage` interface，但 `Start/Complete/Abort/Expire/Verify/fail` 仍会在数据库事务期间调用 MinIO。仅增加 Port 不能消除进程退出和 commit 失败窗口，本阶段直接建立持久化操作与补偿闭环。

新增迁移：

```text
backend/migrations/000011_storage_operations.up.sql
backend/migrations/000011_storage_operations.down.sql
```

`storage_operations` 至少包含：

```text
id / operation_type / idempotency_key(unique)
upload_session_id / object_id
source_bucket / source_key / target_bucket / target_key
state(pending/running/succeeded/failed)
attempt / available_at / lease_until / last_error
created_at / updated_at / completed_at
```

表中不得保存 S3 secret、预签名 URL 或文件内容。操作类型固定为 `multipart.create`、`multipart.complete`、`multipart.abort`、`canonical.ensure`、`staging.delete`、`object.delete`；每种操作定义“目标已经存在/已经删除”时的幂等成功判定。

状态机改造：

1. Start 第一事务写 `upload_session=initializing`、配额预占和 `multipart.create` operation；事务外创建 multipart；第二事务记录 upload ID、标记 session=created 和 operation=succeeded。调用失败或进程恢复由 reconciler 释放配额并清理可发现的 incomplete upload。
2. Complete 第一事务锁定 session/parts，写 `session=completing` 和确定性 `multipart.complete` operation；事务外执行 Complete；第二事务通过 Stat/size 校验后置 session=verifying、operation=succeeded，并写 object.verify command。崩溃恢复重复 Stat/Complete，不能重复扣配额。
3. Verify 先流式校验；第一事务写 `canonical.ensure` operation；事务外执行条件式 Copy/Stat；第二事务创建或复用 FileObject/FileEntry、提交配额、写 file/object events 并标记 operation=succeeded。数据库 commit 失败后由同一 operation 重试，不能立即删除一个可能已被并发事务引用的 canonical object。
4. Abort/Expire/fail 先提交业务目标状态和 `multipart.abort`/`staging.delete` operation，再由同步 fast path 或 reconciler 执行；外部删除失败不能阻止配额状态最终收敛。
5. GC 使用 `object.delete` operation；删除前、每次重试和最终提交前都重新确认 `reference_count=0` 与 object version。引用重新增加时取消本次 operation。

新增 `backend/internal/storageops/` 管理 operation claim、lease、幂等执行和重试。P2 之前 reconciler 由兼容 worker 运行，P2 后只由 `maintenance-worker` 承担；多副本通过 PostgreSQL `FOR UPDATE SKIP LOCKED` claim，不把 Redis 锁作为正确性前提。

测试必须在每个“DB commit 前/外部调用后/第二事务前/operation 状态更新前”注入进程退出或错误，验证重启后：配额不泄漏、FileEntry 不重复、引用不重复、canonical 不误删、staging 最终清理、同一 operation 可重复执行。另做 MinIO 短暂不可用、operation lease 过期接管和 reconciler 多副本测试。

退出门槛：上述外部调用均不再位于长数据库事务内；所有中间状态有超时扫描和确定性恢复路径；故障注入测试与真实 MinIO 集成测试通过，数据库中不存在无法由 reconciler 解释的长期 `initializing/completing/verifying/running` 状态。

回滚：保留 operation 表和新状态；关闭异步 reconciler 时由新版 combined worker 继续执行同一 operation，不允许回滚到不了解新状态的旧二进制，也不通过删除 operation 行恢复。

---

## P1 · 无状态 core-api 多副本

### 目标

让当前 `api` 作为 `core-api` 在多个副本间安全运行，任何副本都不保存必须持久化的本地会话、文件或任务状态。

### P1.1 代码和配置检查

检查并修复：

- `backend/internal/bootstrap/api.go`
- `backend/internal/platform/dependencies/dependencies.go`
- `backend/internal/identity/token.go`
- `backend/internal/identity/service.go`
- `backend/internal/upload/service.go`
- `backend/internal/platform/httpapi/middleware.go`
- `backend/internal/platform/config/config.go`
- `backend/internal/platform/tracing/tracing.go`
- `deploy/prometheus/prometheus.yaml`
- `deploy/local-env.sh`
- `web/src/setupProxy.js`
- `README.md`

必须确认：

1. Access Token 校验只依赖统一的签名配置；Refresh Session 只在 PostgreSQL。
2. UploadSession、幂等键、锁、Outbox 和任务状态全部共享存储。
3. API 不把临时文件、任务队列或用户会话写到容器本地。
4. MinIO 预签名 URL 的公共 endpoint 在多副本下保持一致。
5. 所有副本的 `AUTH_SIGNING_SEED`、`AUTH_ISSUER`、Cookie domain/secure 配置一致。
6. 每个副本都提供 liveness/readiness；readiness 失败先由 Nginx/LB 摘除，再停止接收新请求并等待在途请求。开发 Nginx 的被动失败转移和生产 Gateway/LB 的主动 readiness 摘除必须分别验证，不能把进程存活等同于可接收流量。
7. OTel Resource 增加稳定且逐副本唯一的 `service.instance.id`，默认取容器 hostname/Pod UID，允许显式环境变量覆盖；日志 `instance_id`、Trace Resource 和指标 instance label 使用同一值。

### P1.2 Compose 和入口适配

修改：

- `deploy/compose.yaml`
- `deploy/up.sh`
- `deploy/status.sh`
- `deploy/config-check.sh`
- `web/nginx-app.conf`

实现：

1. 为 `up.sh` 增加 `--api-replicas N`，默认 `1`；只在显式传入时 `docker compose --scale api=N`。
2. 当前 `api` 的固定宿主端口 `${API_PORT}:8081` 会让第二个副本绑定失败。正常 `app` profile 改为只 `expose: 8081`，外部统一经 Web/Gateway；需要宿主直连调试时使用单副本专用 override/profile，不能把固定端口留在可扩容服务上。`API_PORT` 仍是 `deploy/local-env.sh`、`web/src/setupProxy.js` 和 README 中 host-side `local-run/local-frontend` 的开发端口，不得因为 Compose 改用 `expose` 就删除或改成容器内部端口。
3. 当前 Nginx 已有 `resolver 127.0.0.11`、共享 upstream zone 和 `server api:8081 resolve`，保留该实现并补 `max_fails/fail_timeout` 与 `proxy_next_upstream` 策略；验证 DNS 至少返回两个 API 地址、停止/不就绪副本后请求能转移。开发 Nginx 只有被动摘除能力，生产主动健康摘除仍由正式 Gateway/LB 负责。
4. `status.sh` 不再依赖宿主 `API_PORT`，而是逐个 `docker exec <api> wget .../health/ready`，再检查 Web 的 `/readyz`；不能用一个健康副本掩盖其他故障。
5. 日志中增加 `instance_id`（默认容器 hostname，可由环境覆盖），响应或访问日志保留该字段，便于证明请求确实分布到多个副本。
6. `config-check.sh` 已经遍历全部 service container，继续覆盖所有 API 副本，并把 `API_REPLICAS` 及新配置纳入校验。
7. 生产 Gateway 的 TLS、Secure Cookie、`X-Forwarded-Proto` 与 `S3_PUBLIC_SECURE` 作为部署参数验证；开发 Compose 继续 HTTP。
8. Prometheus 不再只静态抓取 `api:8081`；使用 Docker DNS 服务发现、文件服务发现或部署平台原生服务发现逐副本抓取，保留 `service.instance.id`/容器实例标签，并用测试证明两个副本都产生 API 与 runtime 指标。若开发环境的 DNS 只返回轮询地址而无法稳定枚举，使用受控生成的 file-SD 目标，不能以聚合入口冒充逐实例观测。

### P1.3 验证

自动测试：

- 两个 API 副本共享 Refresh Token 轮换；
- 副本 A 创建 UploadSession，副本 B 完成分片和 Complete；
- 两个副本同时使用同一个 Idempotency-Key 只产生一个结果；
- 两个副本同时创建同名文件只成功一个；
- 一个副本停止后，健康路由摘除，文件/目录/上传仍可用；
- 访问日志中的 instance_id 至少出现两个值。

运行演练：

```bash
./deploy/up.sh --no-build --api-replicas 2
./deploy/status.sh
```

使用测试账号和测试文件执行注册、登录、目录、Multipart、下载、删除和 AI 查询。不要在生产数据上做并发冲突演练。

退出门槛：API 副本可任意替换；无本地状态泄漏；健康检查、配置检查和完整测试通过。

回滚：`./deploy/up.sh --no-build --api-replicas 1`；不回滚数据库迁移，除非迁移只存在于空库演练环境。

---

## P2 · Outbox Relay 与资源 Worker 分离

### 目标

把当前 `cmd/worker` 中的 dispatcher、上传清理和多种资源处理拆成可独立扩容的运行角色，同时保留现有 Handler 和 Asynq 语义。

### P2.1 角色和队列所有权

固定队列所有权：

| 角色 | 队列 | Handler |
|---|---|---|
| `outbox-relay` | 不消费队列 | 读取 Outbox，投递 Asynq/Kafka |
| `object-worker` | `object` | 上传强校验、canonical 对象处理 |
| `media-worker` | `media` | 缩略图和媒体变体 |
| `maintenance-worker` | `maintenance` | GC、过期清理、补偿任务 |
| `aisvc`（P2 兼容期） | `ai` | 继续由当前组合 AI 服务执行；P3 再切给独立 `ai-indexer` |

旧 `cmd/worker` 在兼容期保留为组合模式，但默认不再同时启动 dispatcher 和所有资源 Handler。组合模式只用于本地开发。

### P2.2 新增入口和内部接口

建议新增：

```text
backend/cmd/outbox-relay/main.go
backend/cmd/object-worker/main.go
backend/cmd/media-worker/main.go
backend/cmd/maintenance-worker/main.go
backend/internal/platform/worker/role.go
```

要求：

1. `platform/worker` 统一负责 Asynq server 生命周期、Tracing middleware、并发和队列配置。
2. 每个 role 只注册自己拥有的 Handler；取到未知任务时必须进入可观测失败/DLQ，而不是无限重试。
3. Outbox Relay 使用 P0 的 `outbox_deliveries`，每个 transport/destination 独立轮询、重试、退避和指标。
4. Relay 多副本继续使用 Redis Coordination 的 leader lock；丢锁立即停止投递。重复投递必须由 Task ID 和消费者幂等吸收。
5. Relay 不负责执行耗时业务；资源 Worker 不负责轮询 Outbox。
6. Upload cleanup 先作为 `maintenance-worker` 内的独立 runner，保留现有定时扫描和过期释放配额语义；只有其负载、发布周期或故障域确实独立时，再按 Final 方案拆成单独 `upload-cleaner` 进程。

### P2.3 Asynq 任务命名和幂等

将现有事件型任务逐步补齐版本化执行名：

```text
object.verify.v1
media.thumbnail.v1
object.gc.v1
upload.expire.v1
ai.index.v1
ai.reprocess.v1
```

兼容期同时接受旧名称并映射到新 Handler。每个任务 payload 必须包含：

- `event_id` 或 `task_id`；
- 资源 ID；
- 对象/聚合版本；
- trace metadata；
- 不包含大文件内容。

### P2.4 部署和验证

修改：

- `backend/Dockerfile`：支持新 TARGET；
- `deploy/compose.yaml`：增加 `outbox-relay`、`object-worker`、`media-worker`、`maintenance-worker`；
- `deploy/build.sh`、`deploy/up.sh`、`deploy/down.sh`、`deploy/logs.sh`、`deploy/status.sh`；
- `deploy/.env.example` 和 `deploy/.env.no-ai.example`：为各角色提供并发/队列配置。

验证场景：

1. 事务提交后立即停止 Relay，Outbox 保持 pending，恢复后最终投递。
2. Relay 在 Enqueue 成功、delivery 状态更新失败时重启，任务只执行一次有效副作用。
3. 两个 Relay 同时运行，Task ID 冲突不产生重复对象、缩略图或 GC 误删。
4. media-worker 停止时 object-worker 仍能完成上传；AI 停止时文件操作不受影响。
5. GC 在引用重新增加后不删除对象；GC 重试幂等。

退出门槛：每个角色可独立启动、停止和扩容；Outbox pending、投递重试、队列深度、DLQ 和任务耗时有指标。

回滚：Compose 切回兼容 `worker` 组合模式；保留新任务名的兼容 Handler，不能直接删除旧任务数据。

### P2.5 Broker 数据丢失后的任务重建

Outbox delivery 标记 `published` 只证明 Redis/Asynq 曾确认接收，不能证明 Broker 数据在 AOF 损坏、错误恢复或整实例丢失后仍存在。业务未完成状态必须能从 PostgreSQL 重新生成确定性任务，不能把 Redis 当任务存在性的最终事实源。

新增 reconciliation runner，P2 内先归 `maintenance-worker`，至少扫描：

| PostgreSQL 权威状态 | 应恢复的工作 |
|---|---|
| `upload_sessions.status=verifying` 且对象尚未 ready/failed | `object.verify.v1` |
| ready 对象缺少要求的 media variant | `media.thumbnail.v1` |
| AI document/task 长期 pending、building 或 lease 已过期 | `ai.index.v1` / `ai.reprocess.v1` |
| ready/deleting 对象 `reference_count=0` 且无有效 GC 执行 | `object.gc.v1` |
| `storage_operations` 为 pending，或 running 且 lease 已过期 | 对应 maintenance operation |

恢复规则：

1. 每类工作定义稳定 Task ID，至少绑定资源 ID、目标聚合版本和工作类型；同一未完成状态重复扫描只能得到同一 Task ID。
2. 优先复用原 event/delivery；若原 delivery 已 published 或无法安全复用，则创建带 `recovery_reason`、原 event/resource 关联和独立状态的 recovery delivery。所有恢复行为可审计。
3. Reconciler 仍通过 Outbox/Relay 入队，不直接写 Redis；创建 recovery delivery 与确认当前业务状态在同一 PostgreSQL 事务内完成，并受唯一约束保护。
4. Handler 继续以资源当前状态和版本做最终幂等检查；已完成、已取消或版本已前进的任务返回幂等成功。
5. 指标至少包含扫描量、修复量、按类型最老未完成年龄和无法解释状态；定期全量扫描不能只依赖领域事件触发。

验证：清空隔离测试 Redis 的队列/任务元数据、模拟 AOF 恢复到旧时间点和 `published` 后 Broker 丢失，确认上述任务都能重建并最终收敛；重复扫描、Relay 重启和多 maintenance-worker 不产生重复有效副作用。

退出门槛：每一种持久业务中间态都有“识别未完成 -> 创建稳定 recovery delivery -> 重试 -> 收敛”的集成测试；Broker 全丢后不需要人工改业务表即可恢复。完成本门槛且新版 combined worker 回滚演练通过后，才执行 P0.2 所述删除旧 Outbox 投递状态列的 contract migration。

回滚：停止 reconciliation 调度但保留 recovery delivery 和稳定 Task ID 兼容 Handler；不得删除已生成的 recovery 记录，也不得回到无法处理 `storage_operations` 和新 delivery 的旧 worker。

---

## P3 · ai-query 与 ai-indexer 分离

### 目标

保留稳定 gRPC 契约和 PostgreSQL 权限 SQL，把在线查询和离线索引拆成两个独立扩展单元。

### P3.1 代码分层

当前 `backend/internal/ai` 同时包含 Provider、抽取、索引、查询、RAG 和任务。拆分为：

```text
backend/internal/ai/core/        # Document/Chunk/Citation/版本/错误模型
backend/internal/ai/indexer/     # Extract/Chunk/Embed/Enrich/写 ai_* 表
backend/internal/ai/query/       # FTS/pgvector/RRF/Rerank/权限确认
backend/internal/ai/provider/    # DashScope/fake Provider
backend/internal/ai/grpcserver/  # ai-query 的 gRPC 适配
backend/internal/ai/grpcclient/  # API 侧客户端，契约不变
```

可以先通过接口和文件边界拆分，不要复制两套业务逻辑。现有 `ai.Service` 的公共数据结构和错误映射先迁到 `core`，然后逐步让 Indexer/Query 使用窄接口。

### P3.2 进程职责

新增或改造入口：

```text
backend/cmd/ai-query/main.go
backend/cmd/ai-indexer/main.go
```

- `ai-query`：目标态只跑 gRPC Search/Ask、Query Embedding、Rerank、查询侧限流和熔断；不消费 AI 索引任务。兼容发布期可按 P3.3 暂留无独立写逻辑的 Reprocess adapter，观测旧调用归零后移除。
- `ai-indexer`：只消费 `ai` 队列，执行抽取、切块、Embedding、摘要和 `ai_*` 写入；不开放业务查询 gRPC。
- 兼容期 `cmd/aisvc` 保留组合模式，用于单进程开发 profile，并明确日志 `mode=combined`。

### P3.3 gRPC 契约兼容

`Search`、`Ask` 的 HTTP/gRPC 契约不变，API 仍通过 `internal/ai/grpcclient` 调用 query 服务。`Reprocess` 是创建 `async_tasks + Outbox command` 的写命令，所有权移回 `core-api` 的 AI command service：由 core-api 完成 owner/file 权限、幂等键、目标 generation、Task 和 Outbox 的同事务写入，不再把该写操作发给 `ai-query`。

兼容发布顺序：

1. 提取一个可在 core-api 内调用的 `ReprocessCommandService`，并让旧 gRPC `Reprocess` server 暂时委托同一实现，禁止保留两套写逻辑。
2. 部署支持本地 command 的 core-api，但先保留旧 client fallback；验证新旧请求使用同一幂等键只能产生一个 Task/event。
3. 切换 API 只调用本地 command service；至少保留一个兼容发布周期的 gRPC Reprocess endpoint，观测旧调用归零后再标记 deprecated/删除。
4. `ai-indexer` 只消费 `ai.reprocess.v1`/`ai.index.v1`，不承担命令准入和用户授权。

若需要区分服务发现前缀：

```text
/services/ai-query/{instance_id}
```

`ai-indexer` 不加入 query resolver。原有 `owner_id`、folder scope、file IDs、limit、Citation 结构必须保持向后兼容。

调用和重试规则：

- Search：只读，可对 `Unavailable`/`DeadlineExceeded` 重试；stream 已返回消息后不得重试；
- Ask：默认一次，不因网络抖动重复计费；
- Reprocess：HTTP 只重试同一 Idempotency-Key；命令服务返回原 Task，不重复创建 generation/event。

### P3.4 AI 索引幂等和版本

当前 `ai_documents.object_id UNIQUE` 且 chunks 直接归属 document，强制重建会删除旧 chunks 后再替换，不能满足“新版本失败时旧索引继续查询”。新增版本化 migration（编号以执行时最高迁移为准），目标模型为：

```text
ai_documents
  id / object_id(unique) / active_version_id / created_at / updated_at

ai_document_versions
  id / document_id / object_id
  pipeline_version / model_version / generation
  status(pending/building/indexed/failed)
  summary / tags / language
  lease_token / lease_until / last_error
  created_at / indexed_at / retired_at

ai_chunks
  id / document_version_id / ordinal / page_number / section
  content / content_tsv / embedding / token_count
```

唯一键至少覆盖 `object_id + pipeline_version + model_version + generation`；普通重复 delivery 携带并复用确定的 `document_version_id`，不能在每次重试时创建新版本。`force reprocess` 显式创建新的 generation，即使 pipeline/model 未变也不得覆盖当前 active 版本。

构建和切换规则：

1. expand migration 新建 version 表/外键，旧 `ai_documents` 和旧 chunks 继续可读；从现有 indexed document 回填 generation 1 和 active pointer。
2. writer 在新表构建版本；兼容期 query dual-read，优先 active version，无 active pointer 时读取旧布局。不得同时把半成品版本暴露给查询。
3. chunks、摘要和状态全部完成后，在同一 PostgreSQL 事务内把目标 version 标记 indexed 并切换 `active_version_id`；失败只标记 building version failed，旧 active 不变。
4. indexer lease 使用 token fencing；过期 worker 的最终提交因 lease token/version 条件不匹配而失败，不能覆盖新接管者。
5. 完成 backfill、dual-read 校验和 active pointer 切换后，再执行 contract migration 移除旧列/约束；旧版本按明确 retention 延后清理，回滚窗口内不得删除。
6. 对外查询、Citation 和 P6 内容投影都携带 `document_version_id`；任务重试只删除/替换自己的 building version chunks，绝不删除 active 或其他 generation。

测试覆盖：旧数据 backfill；索引构建期间查询继续命中旧 active；新版本失败不遮蔽旧版本；两个 indexer 并发/lease 接管只有一个可切换；重复 delivery 不增 generation；force reprocess 新建 generation；切换事务失败和应用回滚后仍可读取旧布局。

### P3.5 验证

- `ai-query` 副本从 1 扩到 N，在线 Search/Ask 延迟和限流总量符合配置；
- `ai-indexer` 停止时查询仍能读取旧索引；
- AI Provider 错误只让索引状态失败，不改变 FileEntry/FileObject；
- 相同对象重复投递只产生一套有效 chunks；
- 一个 indexer 副本中途退出，另一个副本接管任务并完成；
- Citation 仍只映射当前 owner 可访问的 FileEntry。
- Reprocess 经 core-api command service 创建，旧 gRPC 兼容调用与新 HTTP 路径不会重复建 Task/event。

退出门槛：在线请求和索引积压可以分别观察、分别扩容、分别回滚。

回滚：切回 `cmd/aisvc` combined profile；gRPC 契约和 `ai_*` schema 不回滚，保留旧索引版本。

---

## P4 · Redis 职责隔离和两级缓存

### 目标

把当前一个 Redis 客户端拆成 Broker、Coordination、Cache 三个逻辑职责，先使用独立实例或明确命名空间，再考虑 Sentinel/Cluster。

### P4.1 客户端和配置

修改：

- `backend/internal/platform/config/config.go`
- `backend/internal/platform/dependencies/dependencies.go`
- `backend/internal/platform/dblock/redis.go`
- `backend/internal/ai/rate_limit.go`
- `backend/internal/outbox/dispatcher.go`
- `backend/internal/identity/service.go`
- `backend/migrations/<next>_owner_content_epochs.*.sql`
- `deploy/compose.yaml`
- `deploy/config-check.sh`

目标结构：

```go
type RedisSet struct {
    Broker       redis.UniversalClient
    Coordination redis.UniversalClient
    Cache        redis.UniversalClient
}
```

兼容期：旧 `REDIS_ADDR` 派生三个 client，日志和指标明确 `compat_single_instance=true`；完成独立实例验证后才改默认 Compose。

### P4.2 Cache Port 和键规范

新增：

```text
backend/internal/platform/cache/cache.go
backend/internal/platform/cache/lru.go
backend/internal/platform/cache/redis.go
backend/internal/platform/cache/key.go
```

缓存范围：

- Query Embedding：`embed:{model}:{dimension}:{query_hash}`；
- 文件夹面包屑：`folder:{owner_id}:{content_epoch}:{folder_id}`；
- 分享信息：`share:{share_id}:{share_version}`；
- 搜索候选：`search:{owner_id}:{content_epoch}:{acl_epoch}:{scope_hash}:{query_hash}:{index_epoch}`。

所有 key 必须绑定用户作用域、资源版本、ACL 版本或索引 epoch。最终答案默认不缓存。

L1：每个进程有界 LRU；L2：独立 Redis Cache；回源：PostgreSQL 或派生索引。热点 key 使用 singleflight、TTL 抖动和必要的短锁。

不允许缓存：配额余额、上传最终状态、对象引用计数、最终权限判断、无用户作用域的搜索结果。

### P4.3 失效事件

第一版固定采用 owner 级 epoch，不保留“owner/tree epoch 或枚举子树”的二选一。新增 PostgreSQL 表（迁移编号以执行时最高版本为准）：

```text
owner_content_epochs
  owner_id uuid primary key
  content_epoch bigint not null
  acl_epoch bigint not null
  updated_at timestamptz not null
```

expand migration 为所有已有用户回填初始 epoch 行；注册新用户时在同一事务创建。该 owner 下文件/目录的创建、改名、移动、删除使用 `UPDATE ... SET content_epoch=content_epoch+1 RETURNING` 原子递增；ACL/分享可见范围变化同样递增 `acl_epoch`，若也影响目录/搜索内容则同时递增 `content_epoch`。递增、领域写和事件必须在同一 PostgreSQL 事务，事件 payload 携带提交后的 epoch。粗粒度 owner 失效会降低命中率，但语义确定；只有指标证明它成为瓶颈且迁移/回滚方案完整后，才另立阶段引入 subtree epoch。

将 P0.3 的事件映射到缓存失效：

| 事件 | 失效范围 |
|---|---|
| `file.created/updated/deleted` | 文件元数据、相关 folder、搜索候选 |
| `folder.created/renamed/moved/deleted` | 面包屑、folder 子树的搜索 scope |
| `share.created/revoked` | 指定 share version；自然过期由 `expires_at` 和 TTL 保证 |
| `acl.changed` | ACL version 和搜索候选 |
| `ai.index.completed` | index epoch 相关候选和摘要 |

P7 之前，Outbox Relay 把上述领域事件转换为一个 `cache.invalidate.v1` Asynq 目标，由专用 invalidator 删除 L2 key 并向 Redis Pub/Sub 发布 L1 invalidation；每个 API/ai-query 副本订阅该频道。Pub/Sub 和进程内 epoch 只负责加速收敛，不能承担正确性：每次 L2/事实源回源先读取 PostgreSQL 中该 owner 的 epoch，并据此构造 key；L1 epoch 使用短 TTL，消息丢失后也会重新读 PG。权限结果始终回 PostgreSQL 复核。

失效消息丢失时 TTL/epoch key 必须最终收敛；失效消费者重复执行必须幂等。测试必须覆盖祖先目录 rename/move 后全部后代 breadcrumb 和 scoped search 失效、Pub/Sub 丢消息、多 API 副本保留旧 L1、epoch 事务回滚和乱序失效消息。P7 Kafka 上线后可以把 invalidator 改为 Kafka consumer，但先保持同一失效契约和 consumer name，不能同时让两条链路重复承担事实更新。

### P4.4 验证和回滚

- Cache Redis 停止时，读取回源，写入和权限判断不受影响；
- Broker Redis 停止时，任务进入可见失败/积压状态，但核心事务仍可写 Outbox；
- Coordination Redis 停止时，多 Relay 不得继续假装持有领导权；
- L1/L2 命中率、回源次数、击穿次数、锁异常有指标；
- 权限/版本变化后旧缓存不会返回越权结果。

回滚：设置 `CACHE_ENABLED=false`，保留事实源；删除缓存数据不是回滚步骤。

---

## P5 · PostgreSQL 主备高可用

### 目标

先提升 PostgreSQL 可恢复性和故障转移能力，再考虑读副本和物理分库。业务默认仍使用一个 writer 入口。

### P5.1 部署选择

优先级：

1. 生产托管 PostgreSQL，提供 HA、WAL、PITR 和 failover endpoint；
2. 自建 `Patroni + 有 quorum 的 DCS + Primary + Standby + HAProxy`；
3. 当前开发 Compose 继续单节点 profile，不宣称 HA。

当前 etcd 是 AI 服务发现的单节点，不能直接当生产 Patroni DCS。若共用 etcd，必须独立集群、独立权限和独立前缀，并完成 quorum 演练。

### P5.2 应用连接适配

修改：

- `backend/internal/platform/config/config.go`
- `backend/internal/platform/dependencies/dependencies.go`
- `backend/internal/platform/health/health.go`
- `backend/internal/bootstrap/*.go`
- `deploy/.env.example`
- `deploy/config-check.sh`

阶段一：

- `POSTGRES_PRIMARY_DSN` 指向 HAProxy/托管 writer；
- API、Relay、各资源 Worker、ai-query/ai-indexer 和兼容 aisvc 全部使用同一稳定 writer endpoint；
- 连接池在 failover 后可重建；
- readiness 显示数据库连接和迁移兼容状态。

阶段二（有明确读需求后）：

- 增加只读 pool 和 `ReadDB` Port；
- 只将允许延迟的列表、统计和派生读路由到 replica；
- 写后立即读、权限最终判断、事务内读固定 primary；
- 禁止按 SQL 类型简单地把所有 SELECT 路由到 replica。

### P5.3 备份和演练

必须提供运维脚本或托管平台证据：

- 基础备份和 WAL 归档状态；
- PITR 到指定时间点；
- 主备切换；
- 连接池重连；
- 迁移在旧主/新主上都能安全执行；
- 切换期间上传、分享导入、Outbox 和 AI 任务的失败与恢复语义。

记录 RTO/RPO 实测值，不预先填写目标数字。当前单机 Compose 不执行破坏性 failover。

退出门槛：至少一次 staging failover 和一次 PITR 成功，业务探针、连接池、Outbox pending 和任务重试状态可解释。

应用发布回滚与数据库 failback 分开：应用始终指向稳定 writer endpoint，不直接切物理节点 DSN。一旦新 Primary 接受过写入，禁止把应用简单指回旧 Primary。

数据库 failback 必须按受控流程执行：先 fence 旧 Primary，确认新 Primary timeline 和写入权威；使用 `pg_rewind` 或从备份重建旧主为 standby；等待 WAL catch-up 并验证 replication lag；确有回切需要时执行 planned switchover；最后由 Gateway/连接池和业务探针验证新连接与未决事务。全过程保留旧数据，不在故障演练中删除数据库或卷。

测试覆盖旧主隔离失败/split-brain 防护、连接池在 endpoint 切换后重建、切换瞬间事务结果不确定时携带原 Idempotency-Key 重试，以及 Outbox/配额/分享导入不重复。没有 fencing 证据和 timeline 校验不得执行 failback。

---

## P6 · Elasticsearch/Qdrant 派生索引和 RAG 查询平面

### 目标

在保留 PostgreSQL FTS + pgvector fallback 的前提下，引入 ES/Qdrant 异步投影、影子查询、版本切换和可重建索引。当前 `file_objects` 是跨用户共享的内容对象，而权限、名称和目录属于 FileEntry；索引模型必须保持这两个身份域分离。

### P6.1 索引抽象

新增 Port，不让 `ai/query` 直接依赖具体客户端：

```text
backend/internal/search/index.go
backend/internal/search/es.go
backend/internal/search/qdrant.go
backend/internal/search/fallback.go
backend/internal/search/version.go
```

抽象操作：

- `UpsertContent` / `UpsertChunks`；
- `UpsertFileProjection` / `TombstoneFileProjection`；
- `TombstoneObjectContent` / `DeleteObjectContent`；
- `SearchText`；
- `SearchVector`；
- `Rebuild`；
- `Health`。

ES/Qdrant 返回候选 ID、分数和索引版本，不返回最终授权结论。

### P6.2 索引元数据和版本

增加迁移（具体编号以当前最高版本为准）：

```text
search_index_versions
search_content_projections
search_file_projections
```

内容投影按 object/document version 保存，至少记录：

```text
object_id / document_version_id / chunk_id或ordinal
pipeline_version / model_version / index_epoch
content或content_ref / vector_ref / object_status
backend / target_version / state / attempt / last_error / indexed_at
```

FileEntry/权限元数据投影按 file entry version 保存，至少记录：

```text
file_entry_id / owner_id / folder_id / object_id
file_version / acl_version / name / mime_type / tombstone
index_epoch / backend / target_version / state / attempt / last_error / indexed_at
```

每个 backend、每种 projection 独立记录状态，不能用单一混合的 `search_index_documents` 或一个总状态掩盖 ES/Qdrant、内容投影和 FileEntry 投影之间的部分失败。ES 可按职责使用 `files_v{epoch}` 与 `chunks_v{epoch}` alias/index；Qdrant point/collection 按 chunk/document version 建模，payload 至少包含 `object_id`、`document_version_id` 和 `index_epoch`，不能把 owner/file 权限复制成唯一事实。

投影行为固定为：

1. file rename/move 只更新 FileEntry 投影，不重新抽取或 Embedding。
2. file delete 只 tombstone 该 FileEntry 投影；同一 object 被其他用户/FileEntry 引用时内容投影保持可用。
3. share import 只新增目标用户的 FileEntry 投影，复用已有内容索引。
4. 只有 object GC/deleted 且版本匹配时，才 tombstone/删除该 object 的内容、chunk 和 vector 投影。
5. 重建分别从 PostgreSQL FileEntry/ACL 事实和 active AI document version 恢复；旧 snapshot 写入必须受 aggregate/target version compare 约束，不能覆盖更新的实时事件。

### P6.3 异步投影和影子查询

配置：

```text
SEARCH_WRITE_BACKENDS=postgres|postgres,es_qdrant
SEARCH_READ_BACKEND=postgres|shadow|es_qdrant
SEARCH_SHADOW_SAMPLE_RATE=0.0..1.0
SEARCH_INDEX_EPOCH=<version>
```

分阶段：

1. P6 先新增独立 `search-indexer` 进程角色、`search` Asynq queue 和版本化 `search.project.v1` task；不等待 P7 Kafka。Outbox routing registry 把 file/folder/ACL/object/AI active-version 领域事件转换成 search delivery，Handler 使用固定 consumer name 和 `(event_id, consumer_name)` 去重。
2. 建 snapshot 工具和事实校验器；正式 snapshot 前先打开版本化增量 delivery，让实时事件在 search queue 可见积压，再记录 snapshot start watermark。
3. 从 PostgreSQL 构建内容投影和 FileEntry 投影 snapshot。所有 ES/Qdrant upsert 与 PostgreSQL projection state 都携带 target aggregate/document version；snapshot 的旧版本 compare 失败即跳过，不能覆盖更晚事件。
4. snapshot 完成后排空 watermark 之后的 Asynq 增量；分别校验内容数、FileEntry 数、tombstone、active document version 和每 backend 失败项，再开始 shadow read。
5. 影子查询在主响应之外执行并设独立超时，比较召回集合、权限结果、分数顺序和延迟；影子失败不得拖慢或改变用户响应。
6. Feature Flag 小比例读取 ES/Qdrant，失败立即 fallback；完成回填、alias 切换和回滚演练后再扩大比例。这里的“写 ES/Qdrant”始终是事务后异步投影，不是 API 请求内同步写 PostgreSQL+ES+Qdrant。
7. P7 上线 Kafka 时，对同一 event ID 沿用同一稳定 consumer name。先建立 Kafka delivery 和 cutover watermark；双 transport 窗口由 `event_consumptions` 与版本化 upsert 吸收；Kafka lag 排空并越过 watermark 后关闭 Asynq search delivery。不允许两条路径使用不同 consumer identity 产生两次外部副作用。

### P6.4 权限和 RAG 适配

`ai-query` 的每次查询必须：

```text
PostgreSQL 取得 owner/scope 可访问的 FileEntry <-> object mapping
  -> 候选召回（ES/Qdrant 或 PostgreSQL）
  -> 将 content candidate 与 FileEntry projection/mapping 连接
  -> PostgreSQL 权威过滤 owner/folder/file scope
  -> 校验 file version、ACL version、object status、deleted_at
  -> 去重和 RRF
  -> 受控 Rerank
  -> Evidence / Citation 白名单
  -> LLM
```

不能把索引 payload 的 ACL 过滤当成最终授权。索引故障只能让 AI 降级，不能影响目录、上传、下载和分享。

### P6.5 验证

- 从 PostgreSQL 事实库全量重建 ES/Qdrant；
- 中途失败可从 checkpoint 继续；
- 删除、改名、移动、权限变化都可重放，且 rename/move 不重新 Embedding；
- 同一 object 被两个 owner 引用，删除或移动一方不影响另一方的内容召回；share import 只增加 FileEntry 投影；最后引用 GC 才删除内容投影；
- snapshot 与实时增量交错时，旧 snapshot 不覆盖新事件；Asynq/Kafka 双投递窗口只产生一次有效版本写；
- shadow 查询的权限泄漏必须为 0；
- ES/Qdrant 停止后自动 fallback PostgreSQL；
- 新旧 model/pipeline/index epoch 可并存并切换；
- 召回质量、索引延迟、回填速率和失败重试有实测记录。

退出门槛：至少一次“新索引构建 -> 影子对比 -> Feature Flag 切换 -> 故障回退 -> 再切回”完整演练。

回滚：`SEARCH_READ_BACKEND=postgres`，停止派生索引写入但保留已提交数据和事件；不删除 ES/Qdrant 索引，便于分析和再次切换。

---

## P7 · Kafka 长期事件流

### 目标

为搜索索引、审计和其他长期消费者提供可保留、可重放、多消费者事件流；Asynq 继续负责短期可重试执行任务。

### P7.1 Relay 和 sink 语义

新增：

```text
backend/internal/eventstream/producer.go
backend/internal/eventstream/kafka.go
backend/internal/outbox/kafka_publisher.go
```

继续使用 P2 的 `cmd/outbox-relay`，通过 `--transport=asynq|kafka|all` 或等价配置启动 transport-specific runner；不再创建第二套拥有相同 Outbox 所有权的 `kafka-relay`。生产可把 Asynq/Kafka runner 部署成两个实例组，但选主 key、连接池、指标和 lifecycle 按 transport 隔离。

Kafka runner 只读取 `outbox_deliveries.transport=kafka`，不改变 Asynq delivery 状态。Kafka 不可用时：

- 核心业务事务仍可提交；
- Kafka delivery 保持 pending/failed 并退避；
- Asynq delivery 可独立成功；
- 指标暴露积压和最老事件年龄。

Topic 规划按领域而不是按每个 Go package：

```text
yunpan.file-events
yunpan.folder-events
yunpan.share-events
yunpan.object-events
yunpan.ai-events
```

Key 使用 `aggregate_id`，保证同一聚合内相对有序；跨聚合不假设全局顺序。

### P7.2 消费者、DLQ 和重放

消费者：

- `search-indexer`：P6 已存在，本阶段只把 transport 从 Asynq 切换到 Kafka，不新建第二个业务消费者；
- `audit-consumer`：写审计事实或长期审计存储；
- 其他消费者以后按独立收益增加。

每个消费者必须拥有：

- consumer group；
- `event_consumptions` 去重键；
- 乱序检测和聚合版本处理；
- retry topic 或 DLQ；
- 从指定 offset/时间/事件 ID 重放的命令；
- 业务副作用和消费记录的可追踪日志。

数据库消费顺序固定为：

```text
解析并校验事件
  -> 执行 aggregate/document version 幂等写或记录待执行外部投影
  -> 写 event_consumptions
  -> commit PostgreSQL transaction
  -> commit Kafka offset
```

ES/Qdrant 无法与 PostgreSQL/Kafka 做原子事务，因此外部 upsert 必须包含 target version/index epoch 并可重复；在“外部写成功、消费事务/offset 未提交”窗口重放时，由版本 compare 吸收。不得先提交 Kafka offset 再记录消费结果。

### P7.3 部署顺序

1. 开发 profile 单节点 Kafka，只验证事件契约、生产、消费、DLQ 和重放；
2. staging 验证 broker 重启、consumer 重启、重复消息和分区恢复；
3. 生产再建设多节点副本和监控；
4. 按 P6 的同一 event ID、同一 consumer name 建立 Asynq/Kafka 双投递窗口和 cutover watermark；Kafka 消费越过 watermark、lag 清零并完成 checksum 后，才关闭 Asynq search delivery；
5. 任何阶段都保留 PostgreSQL 事实库重建路径。

退出门槛：Kafka 停止不阻塞核心写入；恢复后积压可排空；重复和乱序事件不会造成重复索引、重复审计或权限错误。

回滚：停用 Kafka delivery 和消费者，Asynq/PG fallback 继续工作；不删除 topic 和 offset 数据。

---

## P8 · 条件式业务边界拆分

### 目标

只有出现可测量的独立扩容、独立发布或故障隔离收益，并且跨服务协议稳定，才拆出业务服务。

### P8.1 拆分前置门槛

以下条件全部满足才允许拆一个领域：

1. 过去一段时间有独立负载指标和扩容需求；
2. 领域数据所有权和写入口已经收口；
3. 对外命令/事件契约有版本管理；
4. 幂等、Outbox、Saga、补偿和回滚已在 staging 演练；
5. 无跨库外键和跨库 SQL 依赖；
6. 可独立部署、健康检查、限流、日志、指标和故障降级；
7. 有回退到 core-api 的兼容开关。

### P8.2 推荐顺序

1. `storage-svc`：上传会话、分片、物理对象和 GC；
2. `content-svc`：目录、逻辑文件、ACL、分享；
3. `account-svc`：用户、Refresh Session；
4. 配额账本最后迁移，不能只移动一张 `quota` 表就宣布 account 拆分完成。

`sharing` 继续和 content 聚合；`upload` 继续和 storage 聚合。没有独立收益时不拆。

### P8.3 数据拆分规则

- 每个服务只写自己的数据库/Schema；
- 跨服务不依赖外键和跨库 SQL；
- 每个服务拥有本地事务和本地 Outbox；
- 跨服务用幂等命令、事件和 Saga；
- 上传、分享导入、删除等跨域配额变化必须显式建模补偿；
- 服务拆分前先实现 anti-corruption layer，让 core-api 可以在同一进程或远程服务间切换。

### P8.4 单领域切换步骤

每次只拆一个领域，后一个领域必须等待前一个领域完成回滚演练：

1. 在原进程内先建立窄的 local Port/local adapter，测试锁定现有语义。
2. 新服务复用同一 command contract，增加 remote adapter；先做 shadow read/响应差异比较，不对业务事实双写。
3. 数据仍在同一 PostgreSQL/Schema 时，先通过单一 writer lease/routing flag 把该领域唯一写入口切到新服务；core-api 只做代理和跨域编排，旧 local writer 禁止并行写。
4. 小流量 canary，注入远程超时、响应丢失、重复命令和服务重启；所有重试携带稳定 command ID/Idempotency-Key。
5. compute rollback 只把流量切回 local adapter，但前提是 local adapter 理解新 schema、Saga 状态和版本；不能借 compute rollback 随意把已写数据切回旧副本。
6. 同 Schema 模式稳定并完成观测后，才允许进入 P9 的物理数据库迁移。data failback 必须依据复制/checksum/writer fencing 单独执行。

### P8.5 Saga 持久模型和业务剧本

新增迁移（编号以执行时最高版本为准），至少包含：

```text
workflow_instances
  workflow_id / workflow_type / business_key(unique) / state / deadline / version
workflow_steps
  workflow_id / step_name / command_id(unique) / state / attempt
  compensation_state / last_error / started_at / completed_at
command_deduplications
  command_id / owner_service / request_hash / result_ref / status
```

状态迁移和本地 Outbox 同事务；每一步明确 command、成功事件、超时、可自动补偿项、不可逆点和人工介入状态。至少落地以下剧本：

| 流程 | 正向步骤 | 补偿/超时 |
|---|---|---|
| 上传 | reserve quota -> create session -> ensure object -> create FileEntry -> commit quota | abort multipart、释放 reservation；canonical 已共享时不得物理删除 |
| 分享导入 | validate share/source -> reserve quota -> create target FileEntry/ref -> commit quota | 删除本次新建 entry/ref、释放 reservation；不得影响源 entry/object |
| 删除/配额 | tombstone FileEntry -> decrement ref/quota -> request GC when zero | 引用恢复则取消旧 GC；事务结果未知用原 command ID 查询/重试 |

补偿本身也必须幂等、可重试和可观测；超过自动恢复期限进入 `manual_intervention`，附资源 ID、最后安全状态和操作 runbook，不能静默标成功。没有完整 Saga 表、故障注入和人工恢复演练，不允许拆 `storage-svc`、`content-svc` 或 `account-svc`。

退出门槛：一个领域按上述步骤完成 shadow、唯一 writer canary、远程故障注入、compute rollback 和 Saga 演练；核心文件操作在目标服务故障时有明确结果，不出现隐式双写。

回滚：切回兼容 local adapter，保留已提交 schema、workflow 和 dedupe 记录；若远程服务已成为唯一 writer，只有确认其写入停止、local adapter schema 兼容并完成 writer fencing 后才能切回。

---

## P9 · 生产基础设施和最终分布策略

### 目标

在业务边界、事件和恢复语义稳定后，才做生产级基础设施扩展和物理数据库拆分。

### P9.1 基础设施施工前置

每一种基础设施是独立施工包和独立发布，不把 P9 整组塞进一个提交。进入前必须有经评审的 ADR 和 runbook，至少写明：实际拓扑与故障域、稳定 endpoint、证书/凭据归属和轮换、备份与恢复、实测 RTO/RPO、容量/限额、监控告警、变更窗口、回滚条件和当班负责人。开发单节点验证不能作为生产 HA 证据。

退出门槛：目标环境、数据范围、依赖方、验收探针和回滚负责人均已具名，备份恢复先于 cutover 演练通过。

回滚：未通过前置评审时不进入数据迁移；任何凭据和用户数据不写入证据文档。

### P9.2 Redis Broker 与对象存储

Redis Broker 迁移：

1. 建立与 Cache/Coordination 分离的稳定 endpoint、权限和容量，按实际平台配置 AOF、replication 和 failover。
2. 在隔离环境验证 broker 主从切换、AOF 恢复、连接重建和旧/新 endpoint 队列观测。
3. 选择“旧队列 drain 后切换”或平台支持的受控复制方案；切换时暂停 Relay 新 enqueue，不停止核心事务写 Outbox。
4. 切 endpoint 后恢复 Relay，排空 Outbox/recovery delivery；执行 P2.5 全量 reconciliation，证明已 published 但丢失的任务可重建。

Redis 退出门槛：切换和故障恢复后所有 PostgreSQL 未完成状态收敛，队列无无法解释任务，Broker 全丢测试通过。回滚：停止新 Relay、切回仍保持权威且未被双写分叉的 endpoint；若两端均接受过任务，先停投递并依 PostgreSQL reconciliation 重建，禁止合并不明队列。

MinIO/对象存储迁移：

1. 从 PostgreSQL 导出被引用 canonical object、staging multipart 和 media variant inventory；记录 key、size、checksum、object/version 状态。
2. 初始全量 copy 后按 checksum/size 校验；默认使用短写入冻结完成最后增量，零停机双写必须另立设计并证明冲突/回滚语义。
3. 冻结窗口确认没有 running `storage_operations` 或将其安全 drain；执行最后增量和 inventory checksum 后，切换 S3 内部 endpoint 与公共 endpoint。
4. 同时验证 presigned upload/download、`S3_PUBLIC_SECURE`、`AUTH_COOKIE_SECURE`、登录/refresh、文件读写、分享访问、variant 和 GC；外部 TLS 终止须验证 `X-Forwarded-Proto`。
5. 原存储只读保留到 observation window 和 restore 演练完成，不立即删除。

MinIO 退出门槛：inventory 数量/size/checksum 一致，读写、预签名、分享、补偿和恢复探针通过。回滚：写冻结，确认新端新增对象可反向同步且 checksum 一致后切回；无法证明一致时保持新端只读并停止切换，不做盲目 endpoint 回指。

### P9.3 搜索、Kafka 和控制面

ES/Qdrant 迁移始终新建 `index_epoch`：从 PostgreSQL snapshot 重建，追平增量并做 checksum/shadow，使用 alias/配置原子切换。故障时 `SEARCH_READ_BACKEND=postgres`；旧 epoch 保留到回滚窗口结束。退出门槛：重建、snapshot restore、节点故障、alias 切换和 fallback 全部演练，权限泄漏为 0。回滚：切回 PostgreSQL 或旧 alias，不反向覆盖 PostgreSQL 事实。

Kafka 生产拓扑按实测负载定义 topic partition、replication factor、`min.insync.replicas`、retention 和磁盘告警；验证 broker/leader 故障、producer ack、consumer offset 备份/恢复、DLQ 与指定 watermark 重放。退出门槛：故障期间核心事务不阻塞，恢复后 consumer lag 清零且无版本倒退。回滚：停止新 Kafka delivery，保留 topic/offset，搜索回到 P6 Asynq 或 PG snapshot 重建路径。

etcd/Patroni 控制面使用有 quorum 的拓扑、独立账号/ACL/前缀；服务发现与 Patroni DCS 不共享无隔离的单节点。验证少数节点故障、失去 quorum 时拒绝危险写/选主、旧 Primary fencing。退出门槛：quorum、fencing、备份和恢复演练均有证据。回滚：恢复到已验证的稳定控制面拓扑，不手工强制两个 Primary 同时上线。

### P9.4 Gateway 和边缘入口

按正式域名和证书建立稳定入口，配置 TLS、请求 ID/Trace、限流、灰度、readiness 主动摘除和优雅缩容；明确 JWT 只做边缘快速校验，最终资源授权仍在服务端。联动验证 `X-Forwarded-Proto`、`S3_PUBLIC_ENDPOINT`、`S3_PUBLIC_SECURE=true` 和 `AUTH_COOKIE_SECURE=true`，覆盖登录/refresh、预签名上传下载、分享链接和多 API 副本摘除。

退出门槛：证书续期、单 Gateway/后端副本故障、灰度回退和所有浏览器关键路径通过。回滚：DNS/LB 权重切回已验证入口；Cookie 与 public S3 scheme 必须与实际浏览器协议一起回退，不能只回退证书代理。

### P9.5 物理分库

只有 P8 拆分门槛和 S8 条件都满足后才进入：

```text
单主库
  -> Primary + Replica
  -> account-db / content-db / storage-db / ai-index-db（按所有权）
  -> 规模确实需要时再按租户/用户/对象哈希分片
```

分库前必须完成：

- 全部跨域写操作清单；
- 外键替代方案；
- 本地 Outbox 和 Saga；
- 历史数据回填、checksum 和可续传 checkpoint；
- shadow read、唯一 command writer cutover 和回滚开关；
- 跨分片查询、迁移和再均衡设计。

每个数据库独立执行：expand target schema -> snapshot/backfill -> checksum -> shadow read -> 停止旧 writer 并 fencing -> 切唯一 command writer -> 通过本地 Outbox/Saga 接续增量 -> observation window -> 旧库只读保留。禁止没有事务语义的隐式双写；确需 change capture 时必须定义顺序、去重、watermark 和停止条件。

data failback 只有在目标停止写、反向增量完整、checksum 一致、旧 writer 已重新 fencing 后才允许；compute rollback 不自动触发 data failback。水平分片不是 HA 替代品，不能把当前跨表 SQL 原样复制到多个分片。

退出门槛：单个领域完成 backfill checksum、shadow 差异为 0、唯一 writer 切换、Saga/Outbox、故障注入和 data failback 演练，再进入下一领域。回滚：保留目标库和旧库只读副本；按已演练 failback 切换，不删除任一侧数据。

## 5. 关键端到端验收剧本

### 5.1 上传、校验、索引

```text
浏览器 Hash
  -> core-api 创建 UploadSession 或秒传 FileEntry
  -> 浏览器直传 MinIO
  -> core-api Complete 第一事务：Session=completing + multipart.complete operation
  -> 同步 fast path / storage reconciler 完成 MinIO operation
  -> 第二事务：Session=verifying + object.verify Outbox
  -> outbox-relay 投递 object.verify
  -> object-worker 流式校验 SHA-256
  -> 写 canonical.ensure operation，reconciler 幂等 Copy/Stat
  -> 本地事务登记 canonical object、FileEntry、引用、配额、object.ready
  -> relay 扇出 media.thumbnail 和 ai.index
  -> media-worker 生成变体
  -> ai-indexer 构建新 DocumentVersion/Chunk/Embedding，原子切 active version
  -> search-indexer 分别更新 content projection 和 FileEntry projection（启用后）
```

验收：在每个 operation 外部调用和事务边界注入退出；重复 Complete、重复 Verify、重复 index、Worker/Reconciler 中途退出、对象存储短暂失败都不会重复扣配额、重复引用、误删 canonical object 或破坏 ready 文件，长期 operation 最终可收敛。

### 5.2 分享导入

```text
owner 创建 share
  -> 只保存 HMAC(Key)
guest resolve
  -> 短期 share access token
guest content
  -> 实时校验 share 和源 FileEntry
登录用户 import
  -> 本地事务锁 share/目标目录/幂等键
  -> 新 FileEntry + 引用 + 配额 + Outbox
```

验收：源文件删除、Share 撤销、Token 过期、重复导入、目标重名、两个导入请求并发都符合既定语义。

### 5.3 AI 查询和问答

```text
Browser -> Nginx -> core-api HTTP
  -> gRPC ai-query（request ID、deadline、owner/scope）
  -> FTS + vector 并行召回
  -> RRF / Rerank
  -> PostgreSQL 权威权限确认
  -> Evidence / Citation 白名单
  -> LLM
```

验收：AI down 时目录/上传/下载正常；Search 可按策略重试；Ask 不重试；索引故障 fallback；任何越权候选和引用均为 0。

### 5.4 搜索投影与共享对象

```text
active AI document version -> content projection(object/document/chunk)
FileEntry/ACL 事实          -> file projection(owner/folder/name/object)
查询 scope                  -> PG 可访问 mapping -> 索引候选 -> PG 最终复核
```

验收：rename/move 只更新 file projection；share import 复用 content projection；删除一个 owner 的 FileEntry 不影响其他引用者；只有最后引用 GC 才移除 object content；snapshot 与增量事件交错、Asynq/Kafka 双消费均不能造成版本倒退或重复副作用。

### 5.5 Redis 和 Kafka 故障

分别停止 Broker、Coordination、Cache、Kafka：

- Broker：Outbox 仍可写；对“入队失败”和“已 published 后 Redis 数据丢失”分别演练，后者由 PostgreSQL reconciliation 重建稳定任务；
- Coordination：Relay 停止投递或重新选主，不能双主持续写；
- Cache：回源事实库；
- Kafka：Kafka delivery 积压，Asynq 和核心写入不受阻；
- 恢复后：Outbox/recovery delivery 和队列积压可排空，重复/乱序可解释，DLQ 可重放；清空测试 Broker 后 upload verify、media、AI、GC、storage operation 均最终收敛。

### 5.6 PostgreSQL failover 与 failback

只在 staging/生产演练：

```text
写入进行中触发切主
  -> fencing 旧 primary，确认新 timeline
  -> 旧连接失败
  -> 应用连接池重建
  -> 新 primary 接受写入
  -> Outbox/任务继续处理
  -> 检查无重复配额、无丢失业务事实
  -> pg_rewind/重建旧主为 standby，追平 WAL
  -> 仅按 planned switchover 执行可选 failback
```

验收：旧主无法在分区期间继续接受应用写；事务结果不确定时以原 Idempotency-Key 重试得到唯一结果；未完成 fencing、timeline 和 replication lag 校验时禁止把应用 DSN 指回旧物理节点。

## 6. 可观测性和指标门禁

### 6.1 必须有的指标

API：

- 副本健康、ready/unready 次数、连接池使用率和连接耗尽；
- HTTP 按 route/status/instance 的请求数、耗时和错误数；
- Prometheus target 和 Trace Resource 可逐个对应 `service.instance.id`，不存在只观测负载均衡入口的“假多副本”；
- gRPC 调用耗时、重试、熔断状态和下游错误码。

异步：

- Outbox pending 数、最老事件年龄、每 sink 投递成功/失败/重试；
- Outbox unrouted event 数和最老年龄；
- Asynq 队列深度、执行耗时、重试、DLQ；
- `storage_operations` 按类型/state 的 backlog、最老年龄、lease 接管和失败数；
- PostgreSQL reconciliation 扫描/修复/跳过/无法解释数和最老未完成年龄；
- Kafka consumer lag、DLQ、重放数。

AI/搜索：

- 索引积压、抽取/Embedding/LLM 耗时、Provider 错误和限流；
- AI active/building/failed document version 数、building 最老年龄和 active 切换失败；
- content projection 与 FileEntry projection 分别按 backend 的 lag、失败、版本跳过和 snapshot 进度；
- PostgreSQL/ES/Qdrant 召回延迟、shadow 差异、fallback 次数；
- Citation 校验失败和权限过滤丢弃数。

数据层：

- PostgreSQL 复制延迟、failover 次数、PITR；
- Redis 命中率、回源、击穿、锁丢失；
- MinIO 对象校验、变体处理和 GC 失败。

### 6.2 Trace 必须能串起来

至少验证以下链路：

```text
HTTP request
  -> PostgreSQL transaction
  -> Outbox event
  -> Asynq/Kafka
  -> worker/ai-indexer
  -> MinIO / PostgreSQL / ES / Qdrant / Provider
```

异步 trace 通过事件信封/Outbox metadata 传播；没有有效 parent 时允许创建新 trace，但必须保留 `event_id`、`task_id` 和 `request_id` 关联。

## 7. 测试分层和每阶段门禁

### 7.1 每个 PR

```bash
test -z "$(find backend -type f -name '*.go' -print0 | xargs -0 gofmt -l)"
(cd backend && go vet ./...)
(cd backend && go test ./...)
(cd web && npm run lint)
(cd web && CI=true npm test -- --watchAll=false --runInBand)
(cd web && npm run build)
```

若 `npm run lint` 的 npm 包装器在当前环境异常，只允许改跑等价的 `(cd web && ./node_modules/.bin/eslint src --ext .js --ignore-pattern build --max-warnings=0)`，并在证据中记录替代原因和原始错误；不能把“命令没跑起来”记作 lint 通过。

同时执行：

- OpenAPI 与实现路由检查；
- 空库 migration up；
- 受影响的 Compose config 检查；
- 不包含密钥和大对象内容的日志扫描。

### 7.2 每个基础设施阶段

- 正常启动；
- 依赖不可用；
- 重复投递/重复消费；
- 进程重启；
- 健康检查和 readiness；
- 回滚开关；
- 证据目录和实际命令记录。

### 7.3 Main/发布前

```text
PostgreSQL/Redis/MinIO 集成测试
单 Chromium E2E：注册、目录、上传恢复、秒传、分享导入、AI 搜索问答
容器构建
至少一次 Worker/Relay/aisvc 故障演练
必要阶段的 race test 和性能 smoke
```

性能只记录：环境、数据量、并发、p50/p95、错误率、配置和 commit；不写“可支撑多少用户”。

## 8. 风险清单和处理顺序

| 风险 | 触发窗口 | 处理 |
|---|---|---|
| Outbox 两个 sink 状态互相覆盖 | Asynq 成功、Kafka 失败或反之 | P0.2 用 `outbox_deliveries` 分离状态 |
| 已 published 任务永久消失 | Redis 数据丢失/AOF 恢复到旧点 | P2.5 从 PostgreSQL 未完成状态生成稳定 recovery delivery |
| Relay 丢锁仍投递 | Redis TTL 到期/网络抖动 | watchdog、丢锁立即停止、Task ID 幂等 |
| AI 索引重复写 chunks | Worker 重试/副本并发 | 确定 document_version_id、lease fencing、版本化 upsert |
| AI 新版本失败遮蔽旧版本 | 重建先删旧 chunks/半成品可见 | active pointer 原子切换；失败版本不改变旧 active |
| 索引返回越权文件 | ES/Qdrant stale ACL | PostgreSQL 权威复核，权限泄漏为硬门禁 |
| 共享 object 索引被误删 | 一个用户删除 FileEntry | content/file projection 分离；object GC 才删除内容投影 |
| P6/P7 双消费重复副作用 | Asynq 到 Kafka 切换窗口 | 同 event ID、同 consumer name、event_consumptions 和版本写 |
| Cache 返回旧权限 | 失效消息丢失 | 版本化 key + TTL 兜底 + 回源复核 |
| 读副本读到旧数据 | 写后立即读/权限判断 | 默认 primary，显式粘滞路由 |
| Kafka 事件乱序 | 分区/消费者重启 | aggregate key、版本检测、重建/回放 |
| MinIO 与 DB 不一致 | 事务内外部调用 | P0.4 记录窗口，后续用补偿状态收口 |
| HA 切主后连接池失效 | DNS/连接缓存 | writer endpoint、连接池重建、failover 演练 |
| DB failback 形成 split-brain | 新主写入后直接指回旧 DSN | fencing、timeline 校验、pg_rewind/catch-up、planned switchover |
| 过早物理分库 | 跨域事务未收口 | P8 全部前置门槛未满足不得拆 |

优先处理顺序：业务事实安全 > 幂等和数据一致性 > 可恢复性 > 独立扩展 > 性能优化 > 服务数量。

## 9. Codex 首次继续施工时的具体动作

下一次真正开始改代码时，严格从 P0.1 开始：

1. 读取本计划和 `ARCHITECTURE_UPGRADE_FINAL.md`，确认当前分支和工作树。
2. 运行 `./deploy/status.sh`、后端测试、前端测试、`go vet`，保存脱敏证据。
3. 统计当前迁移版本、Outbox pending、Asynq 队列、AI 文档状态和对象引用状态。
4. 先建立 `backend/internal/eventcontract/` 的 typed envelope/payload/registry、legacy fixture 和 JSON golden test，冻结 v1 契约后再改 writer。
5. 设计并评审 `000009_outbox_delivery`、`000010_aggregate_versions`、`000011_storage_operations` 的字段、索引、回填、滚动发布和 down migration；三组 migration 分 PR，不能只写 migration 后一次性替换所有进程。
6. 实现 P0.2 的兼容 delivery/Relay，再启用 P0.3 的领域事件，最后完成 P0.4 storage operation/reconciler；每一步都先通过故障窗口测试和回滚演练。
7. P0 全部退出门槛完成后才进入 P1；随后严格按 P1 到 P9 推进。不得因为 ES/Kafka 环境可用就直接开始 P6/P7，也不得在 P2 reconciliation 完成前 contract 掉旧 Outbox 状态列。
8. 任何阶段验证失败，停在当前阶段修复，不跳到下一阶段；不以计划书条目替代实现与运行证据。

## 10. 最终完成标准

只有同时满足以下条件，才可以把本次架构升级标记为完成：

- `core-api` 能无状态多节点运行，副本可健康摘除和优雅缩容；
- Outbox Relay、任务 Worker、AI 查询和 AI 索引可按负载独立扩展；
- MinIO 外部副作用均由持久 `storage_operations` 和 reconciler 覆盖，所有中间态可恢复；
- Broker 已确认任务丢失后，可从 PostgreSQL 未完成状态重建稳定、可审计的任务；
- PostgreSQL、Redis、MinIO 具备已演练的恢复路径；
- PostgreSQL 是唯一业务事实源，ES/Qdrant/缓存/Kafka 是可重建派生平面；
- 内容投影与 FileEntry/权限投影独立，共享 object 的单个引用变化不会误删全局内容索引；
- 事件、任务、消费者、Saga 和回放都有幂等和失败窗口说明；
- 搜索、问答、缓存和索引始终经过权威权限确认；
- 进一步拆分的每个服务都能说明独立收益、数据所有权、协议版本和回滚路径；
- 所有完成结论都有对应源码、测试、运行探针或故障演练证据；
- PostgreSQL failover/failback 已演练旧主 fencing、timeline 校验、连接池重建和 planned switchover；
- 没有用固定副本数、单机性能或未演练的 RTO/SLO 冒充生产高可用。

最终验收不是“拆出多少容器”，而是：

> 在不牺牲业务事务、权限安全、幂等和可恢复性的前提下，计算、任务、搜索、AI 和缓存可以按真实负载独立扩展；任何新服务、新索引或新数据库都能被重建、观测和回退。
