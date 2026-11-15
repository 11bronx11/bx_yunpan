# 异步系统：当前实现、启动与验证

依据根目录 `ASYNC_SYSTEM_CONSTRUCTION_2026-11-16.md` 施工。本文描述当前工作区源码；验证实际日期为 2026-11-15 UTC，不代表现有业务容器已升级。

## 当前链路

```text
API / committer：业务事实 + outbox 事件（同一业务事务）
 → async-control Relay：同一快照安全水位读取
 → 独立控制 PG：inbox + 冻结订阅 + 任务/来源 + 位点（同一控制事务）
 → 有界调度 → RabbitMQ 工作通知 → 独立计算角色
 → 控制 PG 持久结果 → RabbitMQ 32 个结果分片
 → committer：前提复核 + 业务效果/去重凭据 + 后续事件
 → 控制库回报 → ACK
```

业务 PG 是事实权威，控制 PG 是执行权威；双库具有独立 DSN、连接池与迁移历史。控制库支持独立 PG 实例，本次测试在一个实例内使用两个数据库。

默认核心进程为 api、async-control、committer、upload-worker、object-worker、media-worker、gc-worker。API 不构造 broker、不连接控制库，配置和 readiness 不要求二者健康。核心计算角色只有业务只读池，启动时检查角色无业务写权限，并强制只读事务；committer 额外取得业务写池。AI/search 保留原有派生数据写连接，本轮没有搬迁这些数据。

| 入口 | 职责 |
|---|---|
| `internal/asynccontrol` | Relay、任务/结果、调度/恢复、预算/租约、MQ、分片、过期扫描、维护、健康和指标 |
| `internal/asyncprotocol` | 类型化输入、输出、提交结论 |
| `internal/asyncjobs` | 订阅规划、计算适配、事务/批次、历史导入、定向清理、人工重做 |
| upload/objectstore/media 的 async 文件 | 领域前提、业务幂等、失败收尾与删除授权 |
| `bootstrap/async.go`、`async_aux.go` | 核心角色与 AI/search 装配 |

## 正确性与资源边界

- outbox 只追加事件，不写 deliveries；旧 V2/search context 开关不再静默丢事件。订阅及计划版本在接管时冻结。
- Relay 在同一 SQL 快照取得 xmin，按 `(event_xid,seq)` 索引增量读；位点与任务同事务，末尾复核 token。未知契约/永久输入冲突持久隔离，瞬态错误不推进位点。
- 稳定语义身份保留单一预算；新来源、扫描、重投、重启不刷新预算。条件领取、执行代次和 token 阻止旧执行者续租或覆盖结果。执行、派发、失租、结果补发及清理都有上限、期限与退避。
- `computed` 仅表示结果持久化。丢投只补发同一个结果，不重算。业务提交成功而回报失败时，业务凭据阻止重复建文件、加引用或释放配额。永久结果冲突先隔离再 ACK，瞬态失败保留恢复。
- 各控制模块分别持有 PG 租约，Relay 与调度独立运行。续租/结果保存使用保留连接池。
- 新 MQ 是 quorum 工作队列、32 个 SAC 结果队列及 dead 出口，有限传输保护反馈控制库；旧 TTL 队列不参与新重试权威。工作/结果通知携带语义身份、输入摘要、执行代次、提交域、分片布局和 trace；网络入口验证完整契约，领取/提交加载再核对持久身份。
- 成员排序分配固定分片，分片模块租约实现接管。同一 owner/object 提交域只路由一个分片。小批限制条数、字节、等待及事务时间；同域兼容提交合并，批失败有界退回逐条，每项独立回报和 ACK。
- 全局写入速率/事务并发预算不随实例增加。结果高水位触发滞回背压，提交错误/慢提交降低速率，健康等待后逐渐恢复。已运行任务保留续租和结果保存能力。
- 配额热行拆为 `user_quota_usage.occupied_bytes`，预留转已用不改总占用。释放类批次合并一次热行更新。API/后台内容 epoch 分行，缓存与 cursor 在一个 SQL 快照组合两者。

上传由 multipart 创建、完成、哈希/大小验证、必要的世代复制和 committer 最终创建文件推进。计算不持业务事务，不改文件/引用/配额/会话终态。取消、失败、过期和晚到结果都有领域收尾。

GC 先由 committer 复核引用、活跃条目、延迟和世代，进入 deleting 并登记删除意图；GC 删除字节后，再提交 deleted。在线业务扫描仅保留上传会话到期兜底，具有独立租约、keyset 和条数/时间限制；普通到期依靠创建会话时登记的延迟任务。旧业务巡检实现保留兼容测试用途，没有接入新启动链。

复制/缩略图输出包含 task ID 和执行代次，已知废弃输出从执行记录登记有限清理。multipart 创建通过操作专属 staging key 定向找回未知成功；取消/失败/过期登记延迟 sweep，只查询/中止该操作的 multipart，不全 bucket 巡检。

生命周期只给 `staging/` 加规则并保留其他规则。真实测试的 MinIO 会省略 `AbortIncompleteMultipartUpload`，因此配合定向 sweep 和 MinIO 原生 stale-upload 兜底。正式 `objects/compute/`、`variants/compute/` 可能成为有效业务数据，没有自动过期规则。14 天过期/stale-upload 的实际等待清理未做等时验证。

## 迁移与切换

业务新增 033（事务位置/索引、提交凭据、对象世代、占用配额/后台 epoch 回填、过期索引）及 034（multipart sweep 意图类型）。控制迁移独立到 002，包含状态模型、预算/租约、结果、隔离、留存索引与背压恢复状态。033 涉及 DDL/回填和表锁，需切换维护窗口；本次没有认证大表无阻塞迁移。

`up.sh`：补缺失配置键并保留现值/密钥 → 串行构建 → 停止旧写者 → 双迁移/只读角色初始化/导入 → 等导入成功 → 启动新角色。该部署顺序不通过 API 的 Compose 依赖制造 MQ/控制库硬依赖。

`async-import` 要求先停旧 API、dispatcher、worker、AI/search 执行者，有历史事件时校验 `ASYNC_LEGACY_WRITERS_STOPPED=true`。它以原事件/意图身份导入，分开匹配旧扇出目的地，保守合并已耗预算、原上限、期限及成功/停止结论。无法证明的旧输出进入人工处理；水位被长事务阻塞时停止，不能把未接管历史标为完成。

旧 MQ 转移检查无消费者，先 retry/DLQ、后工作队列，每条持久记录后 ACK，不执行旧 payload、不 purge、不删队列。完整切换前保留 pending 标记，控制进程拒绝提前启动。重跑不刷新任务预算。

旧 worker/outbox-relay 命令只是控制角色别名，maintenance-worker 只是 GC 别名；默认不启动。旧表保留为历史输入，旧 dispatcher/taskqueue middleware/storageops/recovery 不再作为在线执行权威。

## 启动与运维

整栈仍用 `./deploy/up.sh`；`--distributed-workers` 兼容为默认明确角色。AI/search/observability 参数保留。status 检查每个核心角色及 readiness，config-check 比较渲染配置和运行容器，只输出差异键名。

宿主机开发，先停旧写者，所有地址从 `deploy/.env` 派生：

```bash
./deploy/init-env.sh --no-ai # 启用 AI 时省略 --no-ai
./deploy/infra-up.sh
./deploy/local-run.sh migrate up
./deploy/local-run.sh migrate control up
./deploy/local-run.sh reader-init
ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-import
```

分别在独立终端运行 local-run.sh 的 api、async-control、committer、upload-worker、object-worker、media-worker、gc-worker，再启动前端。启用 AI 时另起 aisvc/parser，启用搜索投影时另起 search-indexer。只读 DSN 指向业务主库，本轮没有实现读副本回放语义。

Compose 各进程 metrics 是 `:8083`；默认本地端口依次为控制 18083、committer 18084、upload 18085、object 18086、media 18087、GC 18088、AI 18089、parser 18090、search 18091。多副本本地调试可在调用环境覆盖 `WORKER_METRICS_ADDR`。

```bash
./deploy/local-run.sh async-admin inspect <会话/operation/object/task UUID>
./deploy/local-run.sh async-admin redo <task UUID> "明确的修复和重做原因"
./deploy/build.sh async-admin # 可选管理镜像
```

inspect 返回来源事件、执行输出、结果、业务凭据、存储意图及隔离。redo 校验当前前提，创建审计子任务，保留原停止记录和预算；重复同一命令返回同一子任务。维护有界清理终态详情/载荷，保留语义身份、停止预算及未提交结果；业务事件删除同时受已接管事务位点与保留期约束。

## 已执行验证与运行状态

集成使用专用 async_business/async_control 数据库、独立 schema、独立 RabbitMQ vhost/队列前缀和测试 MinIO bucket/key。凭据保存在忽略文件中，未写入源码/报告。

| 验证 | 实际覆盖 |
|---|---|
| 控制协议真实集成 | 晚提交/回滚/子事务水位、原子接管/token fencing、重复领取、失租/最后一次崩溃、结果补发、预算/公平/最低份额、人工重做与留存屏障 |
| 双 committer | 两实例各持 16 分片，停止一个后另一实例接管 32 分片并继续消费 |
| 提交/切换真实集成 | 配额批合并/重复释放、批失败回退、永久冲突隔离、过期扫描去重/终态矛盾、旧预算/成功/耗尽及 MQ 转移、事件留存水位 |
| MinIO 定向窗口 | 创建成功丢结果后恢复 upload ID；活跃会话拒绝 sweep；终止后幂等 sweep 且不删邻居操作；保留已有 lifecycle 规则 |
| 真实核心冒烟 | HTTP 注册/登录，后台不可用时 API 受理；multipart PUT/完成/验证/复制/下载一致/缩略图/暂存清理；提交后回报失败；分享引用/配额、哈希拒绝、取消、过期延期、共享对象 GC |
| AI/search 冒烟 | AI 关闭 HTTP 503；真实控制 PG/MQ 下 fake AI 索引/重处理，用户作业由 committer 终结；PostgreSQL 搜索投影与控制回报 |
| 回归/静态 | build、vet、真实 TEST 环境全量后端回归、改动文件格式化、shell 语法、模板键集合、全 profile Compose、默认拓扑/切换顺序捕获、diff 检查 |

最终代码上的执行结果：

| 命令/检查 | 结果与本地证据 |
|---|---|
| `go build -p=1 ./cmd/...` | exit 0，41.68s；`artifacts/async-build-verified.log` |
| `go vet -p=1 ./...` | exit 0，3.09s；`artifacts/async-vet-verified.log` |
| `go test -p=1 ./... -count=1 -json -timeout=5m` | exit 0，90.82s；386 个顶层测试通过、20 个跳过、0 失败；含子测试共 518 个 pass；`artifacts/async-regression-verified.jsonl`、`async-test-summary.json` |
| `go test -race -p=1 ./internal/asynccontrol ./internal/asyncjobs ./internal/bootstrap -count=1 -timeout=5m -v` | 三包通过，无 race 报告；包含完整真实依赖冒烟和通知身份/世代拒绝测试；`artifacts/async-race-verified.log` |
| 双库迁移命令及重复 `up` | 专用业务库 `version=34 dirty=false`，控制库 `version=2 dirty=false`；`artifacts/async-migrations-verified.log` |
| 部署静态/隔离验证 | 8 个修改脚本语法通过；两模板各 230 个键一致、均可解析全 profile；API 依赖/计算写权限/导入启动顺序核对通过；临时副本验证配置保留与幂等；`artifacts/async-deploy-verified.log` |
| 格式与差异 | 92 个 Go 文件的 goimports/gofumpt 检查通过，`git diff --check` 通过 |

全量回归的 20 个 skip 包含：可选真实解析器、旧合并 worker 冒烟、旧 RabbitMQ 故障/稳定性用例及其子进程 helper、真实 Redis 缓存和 ES/Qdrant 集成；不计入已验证覆盖。旧执行入口已退出在线链路，新控制协议、双 committer、历史消息导入和新核心冒烟均实际使用专用 PG/RabbitMQ/MinIO 运行。

入库用例可设置 TEST 环境复跑：

```bash
cd backend
go test ./internal/asynccontrol ./internal/asyncjobs -count=1 -v
go test ./internal/bootstrap -run '^TestAsyncCoreRealDependencies$' -count=1 -v
go test -race -p=1 ./internal/asynccontrol ./internal/asyncjobs ./internal/bootstrap -count=1 -timeout=5m
go test -p=1 ./... -timeout=5m
```

详细本地日志是忽略目录 `artifacts/async-*.log`。缺环境变量产生的 skip 不算真实验证；本次关键链路使用真实 PG/RabbitMQ/MinIO。AI 使用 fake provider；真实 ES/Qdrant 用例未启用。前端和 Python 解析器实现未改，没有扩展其专项验收。

现有业务容器没有升级/重启，用户业务库、bucket、卷和 `.env` 没有被本次施工迁移/重置；实际运行的是专用测试资源和测试进程。新 Docker 镜像/整栈 Compose 未在用户业务环境启动。

专用测试容器 `bx-async-test-postgres`（宿主机 `27432`）和 `bx-async-test-minio`（`27400`）仍保留；RabbitMQ 复用现有实例中的独立 `bx_async_test` vhost。测试进程已退出，没有启动常驻的新业务角色。未清理旧测试产物、用户队列或数据卷。

跨主机接管、PG/MQ 集群容灾、跨库备份恢复、完整故障矩阵、长压测、容量、生命周期等时执行及大表迁移耗时均未认证。单机验证不能推导这些结论。

11-03 源码事实/进程图、11-12 提案及旧架构方案保留为历史参考。
