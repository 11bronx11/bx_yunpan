# 异步系统：当前实现、启动与验证

2026-11-19 v3 补充：工作区已新增直接执行协议，代码、37/6/12 扩展迁移、10/15 表独立收缩、专用验证及部署边界见 [PG 简化实施记录](../PG_SIMPLIFICATION_EXECUTION_2026-11-19.md)。默认配置仍是 v2；以下 checkpoint/task_deliveries/恢复提交说明属于 v2 兼容路径。不能据此把 v3 改回通用结果暂存，也不能将专用测试视为存量部署已切换。

更新：2026-11-19。当前协议以根目录 `ASYNC_COORDINATION_SIMPLIFICATION_PLAN_2026-11-19.md` 为准；11-16 施工书是旧实现基线。此次实现与验证记录见 [协调裁剪交付报告](ASYNC_COORDINATION_IMPLEMENTATION_2026-11-19.md)。以下链路描述工作区源码，生产栈尚未切换。

## 当前链路

```text
API / 领域 worker：业务事实 + outbox（业务本地事务）
 → Relay：同快照 xmin + (event_xid, seq) 安全水位
 → 控制 PG：inbox + 冻结订阅 + 任务/来源 + 投递意图 + 位点（控制本地事务）
 → 到期发布器 → RabbitMQ 工作通知
 → worker：条件领取 → 计算 → 控制接纳 checkpoint → 领域事务 + receipt
 → 控制确认 + AI 后继释放/作业汇总 → ACK 工作通知
```

业务 PG 是业务事实源，控制 PG 是执行权威，AI PG 是 AI 内容和发布版本权威。MQ 负责积压和消费者分发。核心默认角色为 api、async-control、upload-worker、object-worker、media-worker、gc-worker。独立 committer / ai-committer 已从默认构建、Compose、启动和观测目标退出；保留入口明确拒绝运行。

API 不打开控制库或 MQ，readiness 不要求二者健康。async-control 只有业务只读池；核心 worker 使用独立业务读/写池。只读池启动时检查数据库账号写权限。控制续租与 checkpoint 保存仍走保留池，普通 SQL 不占用该池。AI 后台角色只写 AI PG，不取得业务 SQL 连接。

## 控制协议与故障恢复

任务生命周期为 pending/running/done/stopped，outcome 保存成功、过时、拒绝、失败、隔离和人工处理差异；phase 和 result_id 保存计算、提交、命令及 receipt 恢复进度。

- 接管事务同时写 task_deliveries。发布 claim 有 token/租约；事务外执行路由检查和 publisher confirm，条件回写只修改投递记录。消费者先开始/完成时不会被发布回写倒退。确认丢失可重发同一投递代次；已确认排队的通知不周期补发。
- 领取核对任务身份、投递代次、数据库时间、预算和租约。计算代次只在重新计算时增长。接纳 checkpoint 校验 token/租约并固定结果身份、摘要和冻结后继；保留 token，当前 worker 可以立即提交。
- 领域提交之前复核控制库已接纳该结果；单任务短事务保留 advisory lock、版本/世代前提、配额/引用和 receipt。外部 I/O 不进入业务事务。
- 瞬态失败必须先同事务登记退避和下一次投递意图，再 ACK 原消息。保存失败关闭消费者连接并有界退避，不热 nack/requeue。
- 有 checkpoint 的重试只恢复提交。领域已成功而控制回报失败时复用 receipt；控制成功而 ACK 失败时重复通知直接读持久结论。最后一次提交崩溃可进入一次有截止时间的 receipt-only 恢复，缺凭据则保留结果并标记 needs_attention。
- 计算耗尽或永久失败通过有限 failure.converge / AI 固定失败后继收尾；无法证明提交结论时不能伪造失败覆盖可能已成功的效果。停止任务不会自动重开。

新拓扑为 11 个 `bx.async.v2.work.*` 工作队列和 dead 队列；没有结果队列、SAC 结果配置、分片成员、写许可、owner 公平或结果高水位调度。并发与 prefetch 是每消费者限制，扩副本会增加总量，不承诺旧的全局并发/速率限制。

计算、发布、提交和期限均有限；失租消耗已领取尝试。结果发送/恢复次数、分片、公平、写速率及聚批参数退出配置模板和在线读取，历史表/字段保留供交接。提交尝试上限沿用 ASYNC_MAX_ATTEMPTS，提交窗口由 ASYNC_COMMIT_WINDOW 单独约束。

## 领域职责与维护

`asyncjobs.Committer.Apply` 保留名称，实际为单任务领域提交适配器。upload worker 完成 multipart/安全世代复制并提交；object worker 校验 hash/大小并提交；media worker 计算和发布缩略图；gc-worker 删除字节并提交。

`upload.expire`、`object.authorize_delete`、`failure.converge`、`ai.job.finish` 进入 core-command，由 gc-worker 独立单并发消费者直接执行幂等事务，使用稳定命令 receipt 身份。GC 授权先持久进入 deleting，字节删除在事务外，最后条件提交 deleted。gc-worker 也承担业务 outbox 留存和有限上传到期扫描；事件删除仍受安全接管水位和保留期约束。

控制维护保留已知废弃输出的定向清理、有界终态详情归档；停止预算、语义身份、未提交 checkpoint 和 receipt 保留。仅 staging 配自动过期，正式 objects/compute 与 variants/compute 不配自动删除；multipart 继续使用操作专属 key 的定向 sweep 和原生 stale-upload 兜底。没有恢复旧 dispatcher/recovery 巡检。

## 迁移、交接与运行

当前迁移：业务 000036、控制 000005、AI 000007。控制 5 只新增协议、投递、交接快照/消息证据和旧写者 fence，保留原表/结果/预算。应用连接自动带 v2 协议标记；历史 v1 连接不能修改已切换任务。

已有 v1 控制库：先停全部旧核心/AI 写者并等待执行/模块租约到期，再运行：

```bash
./deploy/local-run.sh migrate control up
ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-handover inspect
ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-handover transfer
ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-handover activate
```

transfer 可重入：原任务快照、保守预算、状态转换及投递意图同事务提交；computed 保留结果 ID/digest/代次，无法证明的结果隔离。旧 MQ 每条先持久交接或隔离再 ACK，无消费者才排空，不 purge、不删队列。activate 检查没有旧协议任务并再次排空；新角色启动检查激活标记。旧消息默认前缀 bx.async.v1，自定义旧前缀通过 ASYNC_LEGACY_BROKER_PREFIX 指定。

`up.sh` 先构建、停旧角色（包含旧 Compose 遗留的两个 committer 容器）、执行迁移和账号初始化、交接/激活，再启动 v2。它不自动重复 AI 历史归档，也不对现有 v1 控制库重跑 async-import。真正从更早的业务执行账本迁移才显式运行 async-import。当前 .env 保留旧前缀时，启动脚本为本次调用派生 v2 默认，不覆盖用户文件。

全新宿主机环境从 .env 派生连接，初始化基础设施、三库迁移及 reader/AI 账号后，在独立终端运行核心/AI 角色。API 18083–18100 的 metrics 分配继续按 local-run.sh；18084/18094 原 committer 端口不再使用。

`async-admin inspect` 展示 lifecycle/outcome、phase、投递、预算、结果、receipt 与来源；redo 校验当前前提并新建留痕子操作，保留原停止预算。指标由控制角色汇总任务表，worker 仅提供本地计数和 readiness。status/config-check 与 Prometheus 不再要求 committer。

控制 5 的 down 明确拒绝协议逆转。新 worker 已开始提交后，不能启动旧镜像来“回滚”；恢复应停写、保留三库和 MQ、核对 receipt/交接证据并向前修复。此次没有认证跨库备份时间点恢复。

## 本轮验证范围

执行结果、日志与剩余边界以 [2026-11-19 报告](ASYNC_COORDINATION_IMPLEMENTATION_2026-11-19.md) 为准。以下历史记录只描述 11-16 旧协议，不作为 v2 的执行证据或当前部署状态。

## 11-16 历史验证与运行记录（旧协议）

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
