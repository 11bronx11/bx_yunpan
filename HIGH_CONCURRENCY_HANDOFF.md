# 高并发核心链路交接文档

- 日期：2026-09-11
- 仓库：`/home/bronx/remote/bx_yunpan`
- 当前状态：P1 修复、隔离真实依赖验证、指标和目录分页已推进；独立 staging 目标库已迁移到 22，初始角色预算已落实并完成回归；完整 staging 发布和容量验证未完成
- 下一阶段顺序（已与用户确认）：迁移目标数据库 -> 落实初始并发预算 -> 部署完整 staging -> 容量验证并校准预算。

## 1. 交接范围

本轮围绕高并发核心路径处理了上传、Outbox、业务缓存、数据库热点索引，以及搜索/AI 派生索引联动。PostgreSQL 仍是目录、文件、权限、配额、引用和上传状态的事实源。

详细计划和测试清单：

- [高并发升级计划](HIGH_CONCURRENCY_UPGRADE_PLAN.md)
- [高并发测试清单](HIGH_CONCURRENCY_TEST_CHECKLIST.md)
- [2026-09-11 隔离验证报告](HIGH_CONCURRENCY_VALIDATION_REPORT.md)
- [旧总架构施工手册](ARCHITECTURE_UPGRADE_FINAL_EXECUTION_PLAN.md)

## 2. 已完成代码

### 上传

入口在 `backend/internal/upload/service.go`：

- `Create` 只在事务内创建 session 和 `multipart.create` operation，HTTP 返回 `initializing`。
- `Complete` 只校验分片、切换 `completing`、创建唯一 `multipart.complete` operation。
- `Abort` 和过期清理只释放配额、切换状态、创建 abort operation。
- MinIO multipart create/complete/abort 和对象校验由 worker 执行。
- succeeded operation 恢复逻辑会把 provider 已成功但业务状态未推进的 session 收敛到正确状态。
- 单任务重试与周期扫描均可收敛 succeeded multipart operation；真实 MinIO + PostgreSQL 状态更新失败窗口已验证。

### Outbox

入口在 `backend/internal/outbox/dispatcher.go`：

- claim 事务不访问 Redis。
- delivery 使用 `sending`、`lease_until`、`lease_token`。
- Redis/Asynq 投递在事务外执行，之后单独 ack。
- 稳定 TaskID 处理投递成功但 ack 前进程崩溃的窗口。
- 过期租约可以被其他副本接管，旧 token 不能覆盖新结果。
- 修复了 ACK 失败回退遗漏 token 的问题，旧 ACK 不再释放新副本租约。

### 业务缓存

入口在 `backend/internal/platform/cache/json.go` 和 `backend/internal/drive/service.go`：

- L1 有界 LRU + Redis L2。
- singleflight 合并同 key 的并发回源。
- Cache Redis client 的连接/读写/池等待和 context deadline 明确限制为 50ms；Load 共享预算，故障回源。
- 已接入目录 children 和文件详情。
- key 带 owner ID 和 PostgreSQL 已提交的 content/ACL epoch，跨 API 和 worker 提交使旧 key 失效。
- 文件详情读缓存前仍验证 owner、删除状态和对象 ready 状态；CreateFile 不再在事务内访问 Redis。
- 缓存命中仍有数据库版本/权限查询，尚未测量总数据库负载收益。

### 数据库索引

迁移文件：`backend/migrations/000022_hot_path_indexes.up.sql`。

覆盖 active upload、文件名冲突、storage operation、Outbox lease、GC、async task recovery 和 trigram 文件名查询。

### 目录分页

实现入口：`backend/internal/drive/service.go`、`backend/internal/drive/http.go`、`web/src/services/drive.js`、`web/src/pages/Drive.js`、`web/src/components/MoveFileModal.js`。回归入口：`backend/internal/drive/service_page_test.go` 及对应前端测试文件。

- children HTTP 默认 50 项，`limit` 范围 1..100，支持 `name_asc` / `name_desc`。
- 按目录优先、PostgreSQL 名称排序、ID 排序执行 keyset；文件游标之后不再查询目录。
- cursor 绑定 owner/folder、content/ACL epoch 和排序。非法 cursor 返回 422，epoch 变化返回 409。
- 首页缓存使用同次读取的 epoch 构造 key；返回缓存前再次检查 epoch。后续页不缓存，分页 SQL 接收 HTTP context。
- Drive 和移动目录树按需加载，支持取消旧请求；Drive 的过期游标自动重载首页。
- OpenAPI 已同步。真实 PostgreSQL、多页 HTTP、前端回归及桌面/手机浏览器交互通过，详见验证报告第 7 节。

### 搜索和 AI 联动

- `backend/internal/search` 增加 Elasticsearch/Qdrant adapter、索引版本、checkpoint、回填、激活和回滚。
- 外部索引结果会重新走 PostgreSQL owner、active version 和 object 状态校验。
- AI 搜索支持外部读、shadow、PostgreSQL fallback 和并行 hybrid recall。
- AI parser 增加大小限制、质量检查、错误分类和可重试判断。
- AI 事件带 run、hash、文档版本、pipeline/model/parser 版本等元数据。

## 3. 已验证结果

以下为 2026-09-11 已完成的实测记录。独立 staging 迁移和预算验证见本次新增的第 8 节；其他数据库版本、预览地址及进程信息是上一轮结束时的快照，恢复工作时需核对。

首轮 P1 隔离验证，证据：`artifacts/high-concurrency/20260911T071039Z-1206723/`：

```text
go test ./...                         207 passed / 1 skipped / 47 packages
go vet ./...                          passed
cache/drive/outbox/dbmetrics/upload race  56 passed / 5 packages
双 API HTTP 缓存与指标 smoke           passed
Prometheus 告警规则触发/恢复测试        passed
```

真实 Redis 暂停 1 秒时约 51ms 回源；真实 MinIO 完成后注入 PostgreSQL 状态更新失败，重试后事件、文件、引用和配额不重复。最后调整积压年龄查询后，另跑 dbmetrics race 通过。唯一跳过项需要真实 ES/Qdrant。

`000022` 在隔离数据库完成 22 -> 21 -> 22，5 万行合成数据已做 EXPLAIN；实际索引选择和证据边界见验证报告。当前开发数据库仍为迁移 21，尚未正式应用。上一轮测试容器曾停止，分页验证时已重新启动。

分页增量证据：`artifacts/high-concurrency/20260911T075328Z-1381869/`。

```text
go test ./...                          225 passed / 1 skipped / 47 packages
cache/drive/outbox/dbmetrics/upload race   74 passed / 5 packages
前端 Jest                              42 passed / 14 suites
go vet、前端 lint/build、双 API smoke、告警测试   passed
Playwright 桌面/手机分页、移动、409 自动恢复       passed
```

本地预览保留在 `http://127.0.0.1:3001`，仅连接隔离 API `127.0.0.1:18091` 和测试数据库，AI/worker 未启动。原开发入口 3000 未发布新版本。预览进程及停止方式见验证报告第 7 节。

## 4. 尚未完成

- 在已迁移的 staging 目标数据上补充 `EXPLAIN (ANALYZE, BUFFERS)` 检查索引。
- 完整上传 HTTP 故障、MinIO create 歧义窗口、真实 relay 进程退出/恢复、broker 中断恢复。
- 部署并实际抓取已新增的缓存、Outbox/storage backlog、SQL pool 指标，验证告警通知。
- 根据容量验证校准 API、relay、storage、AI 的连接池和 worker 并发预算（初始预算已在第 8 节落实）。
- staging 压力 Smoke 和容量边界报告。
- 重新构建并部署 API、relay、object worker、maintenance worker。

这些事项完成前，不要把本地测试结果写成生产 QPS 或容量结论。

## 5. 继续工作入口

按本次与用户确认的顺序恢复。目录分页已经完成，不再作为待实现事项。下列步骤是后续工作，本次交接没有执行。

准备工作：阅读验证报告、查看工作树，明确本次发布范围与目标环境，核对目标数据库、服务副本数和机器资源。独立 staging 目标库和初始预算已完成；完整新版应用仍未部署。

### 第一步：迁移目标数据库

- 对准备运行新版服务的目标库执行 `000022`，确认版本达到 22 且 `dirty=false`，核对 Outbox 租约字段、状态约束及索引。独立 staging 已完成此项，证据见第 8 节。
- 如果使用独立 staging，迁移 staging 库即可；原开发库不必因此升级。原开发库保持 21 不表示独立测试库没有完成迁移。
- 结合目标数据执行 `EXPLAIN (ANALYZE, BUFFERS)`；旧合成数据的结果不能替代目标库检查，分页查询也需单独观察。
- 完成标准：目标库结构满足新版服务要求，迁移结果及 SQL 计划有记录。目标库迁移已完成；此时尚未启动完整新版应用。

### 第二步：制定并落实初始并发预算

- 明确 API、relay、object/media/maintenance worker、AI 等本次部署角色的副本数、每实例连接池上限和任务并发。独立 staging 的初始表见第 8 节。
- 按“各角色副本数 × 每实例连接上限”汇总数据库连接额度，并计入其他客户端和必要的运维余量。worker 任务并发与数据库连接数分别配置，不能简单等同。
- 将预算落实到实际部署配置，核对各进程最终生效值，避免全部角色沿用同一个通用默认值。
- 完成标准：角色、副本数、连接上限、任务并发及选择依据形成可检查的配置表。独立 staging 已完成初始预算；它尚未证明容量。

预算方案可以在迁移前设计；两者没有严格的先后依赖。但目标库迁移和初始预算配置必须在启动完整新版服务前就绪。

### 第三步：部署完整 staging

- 按确认的发布范围构建并部署配套版本的 API、relay 和各类 worker，使用已迁移的目标库及第二步预算。
- 本机继续顺序构建，沿用 `GOMAXPROCS=2`、`GOFLAGS=-p=1` 的受控构建方式。
- 检查健康状态、目录分页、完整上传及持久任务收敛，实际抓取缓存、队列积压、SQL pool 指标，并验证告警通知。
- 在该环境补完未验证的 MinIO create 歧义窗口、完整上传 HTTP 故障、真实 relay 进程退出/恢复、broker 中断恢复。
- 完成标准：整套服务在目标配置下正常协作，指标可观测，相关功能与故障恢复有真实证据。本地 3001 预览仅运行网页和隔离 API，不能替代此步骤。

### 第四步：容量验证与预算校准

- 运行目录读、上传幂等、混合搜索及后台任务同时运行的压力场景，逐步增加负载。
- 记录机器配置、数据规模、服务副本数、并发数、吞吐、p50/p95/p99、错误率、数据库连接等待、CPU/内存和积压恢复时间。
- 根据瓶颈调整连接池、worker 并发及副本数，再验证；部署前的初始预算由此得到校准。
- 完成标准：明确指定环境和配置下的稳定范围、瓶颈及失效边界，不把测试通过数量或本地 smoke 结果转换为生产容量。

隔离回归脚本仍可用于有针对性的重验：`HC_GO_BIN=/home/bronx/.local/go/bin/go bash deploy/high-concurrency-check.sh`。它会建立独立测试数据库，不会替代目标库迁移、完整 staging 发布或容量验收。

## 6. 回滚边界

- 业务缓存可用 `CACHE_ENABLED=false` 关闭，回到 PostgreSQL。
- 不删除数据库、Redis、MinIO 对象、队列或 Docker volume。
- `000022` 回滚前确认旧版本没有依赖新增列和索引。
- 回滚前停止新 relay 并处理 sending 行；旧状态约束不接受 sending，不能直接在线 down。
- 上传 operation 保留在数据库中，worker 可暂停后重试，不恢复请求内 MinIO 调用。

## 7. 工作树说明

当前工作树包含其他 AI/RAG 文档和构建产物。它们不属于本轮高并发主线，不要在交接时清理或回滚；继续工作前先查看 `git status`，保留用户已有修改。

## 8. 独立 staging 迁移与预算结果（2026-09-11）

- Compose 项目：`bx-yunpan-staging`，目标数据库：`hc_staging`，宿主端口：`25432`。未启动完整应用服务。
- 迁移结果：`schema_migrations=22|dirty=false`；`max_connections=100`、`superuser_reserved_connections=3`；9 个新增热点索引和 `lease_until`/`lease_token` 已存在。
- 配置文件：`deploy/staging.env.example`、`deploy/compose.staging.yaml`；预算检查：`node deploy/staging-budget-check.mjs`。
- 初始预算：API 2x12、relay 2x4、object 1x8、media 1x4、maintenance 1x4、AI query 1x8、AI indexer 1x6、search indexer 1x4，总应用连接上限 66；另预留迁移 2、运维 16、超级用户 3，未分配 13。
- worker 任务并发分别为 object 4、media 2、maintenance 2、AI indexer 2、search indexer 2；relay 使用 leader lock 和 batch 20。`worker`/合并 `aisvc` 副本固定为 0。
- 回归：Go 全量 225 通过/1 跳过，专项 race 74 通过，`go vet`、迁移往返、双 API HTTP smoke、前端 Jest 42 通过；真实 ES/Qdrant 集成仍跳过。前端 lint 直接执行原脚本通过；本机命令包装路径曾误扫生成文件，未据此修改业务代码或原 lint 脚本。
- 证据目录：`artifacts/high-concurrency/20260911T090722Z-staging-prep/`、`artifacts/high-concurrency/20260911T090856Z-45945/`。staging PostgreSQL 容器保留，未删除 volume。

## 9. 本次停止点

本次按用户要求完成独立 staging 目标库迁移、初始并发预算和单元/回归验证后结束。没有部署完整新版应用，也没有运行容量压测。下次继续时，从第 5 节第三步的完整 staging 发布开始。
