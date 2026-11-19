# AGENTS.md

BX YunPan：React 18 + Go（Gin）的私有云盘，带分片上传、秒传、分享、缩略图和可选的 AI 检索与 RAG 问答。本文件给在这个仓库里工作的 coding agent 用，只写要跨多个文件才能看明白的东西，细节以源码为准。

## 先知道这几件事

- **README 有一部分是旧的。** 任务队列在 2026-10-08 从 Asynq 换成了 RabbitMQ，README 的架构图和「Redis/Asynq Worker」等说法没跟上。异步系统使用独立控制 PG + worker 自行计算与提交；2026-11-19 新增可选 v3 直接执行协议，默认配置仍为 v2。v3 代码、迁移、交接及验证边界见根目录 `PG_SIMPLIFICATION_EXECUTION_2026-11-19.md`。Redis 只做缓存与限流，不负责异步选主。`docs/ASYNC_SYSTEM_IMPLEMENTATION_2026-11-16.md` 和 `docs/AI_SYSTEM_IMPLEMENTATION_2026-11-16.md` 保留 v2 兼容链路说明；两个 11-03 文档是更早源码快照。
- **`docs/` 默认被 `.gitignore` 忽略**，只有白名单里的文件入库；`artifacts/` 整个忽略。新文档要入库，得在 `.gitignore` 加一行 `!docs/<文件名>`。
- **仓库里没有 CI 配置**（无 `.github/`），格式化、vet、测试都靠本地跑。
- 写验证报告的惯例：只写实际执行过的结果。单机 smoke、fake provider、静态检查不写成容量或高可用结论。

## 常用命令

后端，在 `backend/` 下，要求 Go 1.25.13+：

```bash
go build ./cmd/...
go vet ./...
go test -p=1 ./...                                   # 全量；集成用例缺环境变量时自动 skip
go test ./internal/outbox -run 'TestName' -count=1   # 单个测试
make fmt             # gofumpt + goimports
make lint            # golangci-lint，仓库里没有配置文件，用默认规则
make proto           # 改了 api/proto 后重新生成 internal/ai/pb、pbv2 和 internal/core/pb（生成物入库）
make proto-check     # 校验生成物没有漏提交
go generate ./api    # 改了 api/openapi.yaml 后重新生成 internal/contract/api.gen.go
```

HTTP handler 不依赖 `internal/contract` 的生成代码，改接口时要同时手改 `backend/api/openapi.yaml`（它被嵌入二进制，由 API 的 `/docs` 展示）。

集成测试靠环境变量开启，没设就 skip：

- `TEST_POSTGRES_DSN`：专用业务测试库；绝大多数集成用例
- `TEST_CONTROL_POSTGRES_DSN`：专用独立控制测试库
- `TEST_AI_POSTGRES_DSN`：专用独立 AI 测试库；`TEST_AI_SPLIT=true` 与真实 parser/etcd/ES/QD 等变量开启新三库链路
- `TEST_ASYNC_CORE=true` + `TEST_POSTGRES_READONLY_DSN`：开启真实 HTTP/MQ/MinIO 核心冒烟，读角色必须无业务写权限
- `TEST_RABBITMQ_URL` + `TEST_RABBITMQ_MANAGEMENT_URL`
- `TEST_MINIO_ENDPOINT` / `TEST_MINIO_ACCESS_KEY` / `TEST_MINIO_SECRET_KEY`
- `TEST_REDIS_ADDR`
- `TEST_SEARCH_ES_ENDPOINT` / `TEST_SEARCH_QDRANT_ENDPOINT`

夹具的隔离程度不一样：identity、outbox 等包在目标库里建独立 schema（RabbitMQ 用独立 vhost），结束时删除；storageops、recovery、upload 并发、sharing、search 等夹具使用独立迁移 schema；仍有兼容用例读写目标库 `public`，不能据此把整个测试套件指向开发库。outbox 的夹具还用 `CREATE TABLE ... LIKE public.<表>` 克隆表结构。所以要用一个跑过迁移的专用测试库，不要指向开发库。

迁移文件是 `backend/migrations/NNNNNN_name.{up,down}.sql`（golang-migrate，源码到 000037；控制迁移独立放在 `backend/control-migrations/`，到 000006；AI 迁移在 `backend/ai-migrations/`，到 000012；旧 AI 拆库归档在 `backend/archive-migrations/`）。v3 收缩单独放在 `control-contract-migrations/` 和 `ai-contract-migrations/`，不会随普通 `up` 自动执行。源码版本不代表运行库已经升级：

```bash
./deploy/local-run.sh migrate up        # 业务库；还支持 down-one、version
./deploy/local-run.sh migrate control up # 独立控制库
./deploy/local-run.sh migrate ai up      # 独立 AI 库
./deploy/local-run.sh ai-reader-init     # query / compute / commit 独立账号
```

前端，在 `web/` 下，要求 Node.js 22+：

```bash
npm run lint
CI=true npm test -- --runInBand                    # 全量
CI=true npm test -- --runInBand src/App.test.js    # 单个文件
npm run build
```

解析器（Python），在 `backend/` 下，依赖见 `requirements-parser.txt`：

```bash
python3 -m unittest parser_worker.test_parsers -v
```

整套 Docker 栈：

```bash
./deploy/up.sh [--no-build] [--distributed-workers] [--ai-split] [--redis-split] \
               [--search-indexer] [--search-backends] [--observability]
./deploy/status.sh          # 容器状态，并检查 Web、API、对象存储代理链路
./deploy/config-check.sh    # 运行中的容器与 deploy/.env 是否一致
./deploy/logs.sh [服务...]
./deploy/down.sh            # 保留数据；加 --volumes 会删掉数据库和对象
```

入口：Web `http://127.0.0.1:3000`，API `http://127.0.0.1:8081`（`/docs` 是接口文档），MinIO Console `:9001`，RabbitMQ 管理界面 `:15672`。

只用 Docker 跑基础设施、Go 和 React 跑在宿主机：

```bash
./deploy/init-env.sh [--no-ai]    # 生成 deploy/.env；已有文件只补缺键，保留现值
./deploy/infra-up.sh              # postgres、async-postgres、redis、minio、rabbitmq
./deploy/local-run.sh migrate up
./deploy/local-run.sh migrate control up
./deploy/local-run.sh reader-init
ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-import # 先停旧写者
./deploy/local-run.sh api         # 各角色在独立终端运行，详见当前启动说明
./deploy/local-frontend.sh        # :3000
```

`local-run.sh` 和 `local-frontend.sh` 从 `deploy/.env` 派生宿主机地址，包括 RabbitMQ、业务只读 PG 和控制 PG；不要另外维护配置。默认宿主机 metrics 端口由角色分配为 18083–18100，可用调用环境中的 `WORKER_METRICS_ADDR` 覆盖。`worker` 只是 `async-control` 的兼容别名，不再启动计算角色。
## 架构

### 进程与部署形态

`backend/` 是一个 Go module。`cmd/*` 的入口装配在 `internal/bootstrap`。业务 PG 是唯一业务事实源；独立控制 PG 是唯一任务执行权威。MinIO 保存字节；Redis、Elasticsearch、Qdrant 是可重建的加速层。控制库可以位于独立 PG 实例，测试使用同一实例的两个数据库不等于实例资源隔离。

核心默认进程是 `api`、`async-control`、`upload-worker`、`object-worker`、`media-worker`、`gc-worker`。`--distributed-workers` 现为默认形态的兼容参数。AI 默认是 `core-internal`、`ai-query`、`ai-content-worker`、`ai-embedding-worker`、`ai-summary-worker` 和 parser，`--ai-split` 是兼容参数；ES/QD projector 分开启动。旧 `worker` / `outbox-relay` 命令只映射控制角色，`maintenance-worker` 只映射 GC；不要与对应新角色一起启动。

核心 worker 通过 `dependencies.OpenAsync` 取得控制池、保留控制池、业务只读池及独立业务写池；async-control/管理角色仍无业务写池。业务只读角色必须无写权限。API 不打开控制 PG 或 broker；其配置校验和 HTTP readiness 不要求二者健康。AI 在线角色只连接独立 AI PG，通过 core RPC 获取权限、对象和短期授权；没有业务 SQL 池。query 仅能读发布视图；AI worker/projector 使用 `AI_COMMIT_DSN` 同时写 staging 和发布。`AI_COMPUTE_DSN` 只保留历史兼容账号；两个 committer 入口明确拒绝启动。

### 异步链路与修改入口

以下 1–8 描述默认 v2 兼容路径。显式设置 `ASYNC_PROTOCOL_VERSION=3` 并完成交接激活后，核心使用 `RunDirectWorker` / `asyncjobs.DirectWorker`，AI 使用 `aisystem.DirectWorker`：领取 → 查稳定领域回执 → 计算并提交领域事务 → `Store.Complete` → ACK。v3 不写控制 results/executions/continuations/deliveries；投递字段在 tasks 内，完成事务登记后继和作业引用；最后一次恢复只能读回执，无法确认时停止为 needs_attention。AI 类型化计算值直接发布，摘要只写专用 ai_summary_nodes，外部投影在写前登记清理线索。

v3 切换必须停旧写者，先处理旧已接纳结果，再用 `async-handover` 和 `ai-direct-admin` 保留预算、摘要节点和消息证据并激活。旧结果或 observers 未核清时工具会拒绝切换/归档。前缀必须与旧队列隔离，数据库连接同时带协议标记；不能只靠队列前缀。历史兼容字段/参数在 v2 尚需运行时保留，实际 `.env` 不自动改为 v3。

1. `outbox.AddTyped` 在事实事务内只追加 `outbox_events`，不写 deliveries，旧 V2/search context 开关不再静默丢弃事实事件。
2. `asynccontrol.Relay` 用同语句快照的 `xmin` 和 `(event_xid,seq)` 索引读取；inbox、冻结计划、任务/来源、投递意图、位点在控制库同事务接管。不能改回只按自增号推进。
3. 任务契约与预算在 `asynccontrol/model.go`，订阅在 `asyncjobs/planner.go`，计算在 `asyncjobs/compute.go`；共享载荷在 `asyncprotocol`。新增类型要同步这些入口与领域提交函数。
4. 控制库管理有限计算、发布、提交预算、总期限和执行租约；生命周期为 pending/running/done/stopped，outcome 保存领域结论。MQ 拓扑为 `bx.async.v2.work.<queue>` 和 dead；不声明结果队列，不保留 owner 公平、结果分片、全局写许可或 TTL 重试。并发/prefetch 是每消费者限制。
5. worker 条件领取、续租、保存不可变 checkpoint（保留 token），直接提交领域效果与 receipt，再回报控制库，最后 ACK 工作通知；已接纳结果只恢复提交。预算耗尽后的最后一次恢复只查 receipt，无法证明则保留结果并停止。纯命令使用稳定 receipt 身份，不伪造计算结果。
6. 领域提交在 upload/objectstore/media 的 `async_commit.go` 或 `async.go`；`asyncjobs.Committer.Apply` 仅组织单任务短事务；`core-command` 由 gc-worker 独立小并发消费，gc-worker 还负责安全位点约束的业务 outbox 留存。事务内不能调用 MinIO。
7. `storage_operations` 现在保存业务意图/提交结论；旧执行租约与预算只供导入及兼容测试。新启动链不调用旧 dispatcher、taskqueue middleware 或 recovery 巡检。在线业务扫描仅保留 GC 的上传会话到期兜底。
8. `async-import` 在旧写者全部停止后导入保留事件/操作，继承旧预算、成功及停止结论，逐条持久转移/隔离旧 MQ 消息后 ACK，不 purge。`async-admin inspect` 定位任务，`redo` 校验当前前提并创建留痕子任务；不会重开原停止任务。

v1 控制协议升级用 `async-handover inspect/transfer/activate`：停旧写者、等待旧租约到期，分批保留 checkpoint/预算/快照并逐条转移旧消息后 ACK。数据库 trigger 拒绝旧镜像写入已切换任务；不能用旧镜像或 down migration 回滚。不要对已有 v1 控制库重新跑核心 async-import。

长期输入、结果和执行详情有界维护；终态语义屏障、停止预算和历史未提交结果保留。`internal/workflow` 仍没有接入启动链，不要引入第二个任务权威。`docs/ASYNC_CONTROL_PLANE_DESIGN_2026-11-12.md` 是历史提案，`ASYNC_COORDINATION_SIMPLIFICATION_PLAN_2026-11-19.md` 是 v2 基线；v3 依据根目录 CONTROL_PG / AI_PG 两份 2026-11-19 计划及实际实施记录，11-16 施工书继续作为历史基线。

### 上传与对象模型

文件字节不经过 Go API：API 只签发预签名 URL，浏览器经同源的 `/storage` 直传 MinIO。逻辑文件（`file_entries`）和物理对象（`file_objects`，按 SHA-256 加大小内容寻址，带 `reference_count`）是分开的。秒传和分享导入都只是新增 entry 并给对象加引用。

创建、完成、中止这几个请求只改 PostgreSQL 状态，对 MinIO 的调用都由 worker 完成，所以一次分片上传是被异步任务一步步推进的：

`Create`（会话 `initializing`，登记 `multipart.create` 操作）→ upload 队列建好 multipart → 客户端传分片并确认 → `Complete`（`completing`，登记 `multipart.complete` 操作）→ `verifying`，发出 `object.verify_requested` → object 队列只校验 SHA-256/大小 → object worker 提交时校验前提，必要时登记安全世代复制 → upload 队列复制字节 → upload worker 建 entry、加引用、结算配额，并发出 `file.created` 和 `object.ready` → media worker 计算并发布缩略图；AI 角色建索引。

引用归零的对象由 `object.gc_requested` 延迟回收，延迟由 `OBJECT_GC_DELAY` 控制，默认 24h。先由 gc-worker 的命令消费者授权进入 deleting，GC 删除对应世代字节后，再由 gc-worker 提交 deleted。暂存仅对 `staging/` 配过期规则；MinIO 省略 multipart lifecycle 时，使用操作专属 key 的延迟 sweep 和原生 stale-upload 兜底，不给正式 compute 输出设置自动过期。

### AI、解析与搜索

- API 的 `aigateway` 走 ai/v2 gRPC + etcd resolver；`core-internal` 提供带服务身份和 scope/processing grant 的 core/v1 RPC。AI 无业务 SQL、跨库 JOIN 或业务作业写连接。
- `AI_ENABLED=false` 时内容搜索/问答/重建返回 503，core 文件名搜索仍可用。文本准备不要求模型；`AI_VECTORS_ENABLED`、`AI_SUMMARY_ENABLED`、`AI_ASK_ENABLED` 各自开关。
- 控制 effect target 分 core / AI；staging 只有经控制接纳、AI worker 本地发布并写 receipt 才可见，控制确认和冻结后继释放在同一事务。
- `aisystem/planner.go`、`compute.go`、`publish.go`、`projection.go`、`query.go` 分别负责计划、计算、发布、独立 ES/QD 投影、检索问答。source、text、vectors、summary 的 desired seq 和 active pointer 分开，失败快照不能覆盖新成功。
- 独立父子片由 `ai/rag/parent_child.go` 生成，子文本不要求向量。全文请求不调用模型；semantic 需要兼容 query embedding，hybrid 可文本降级。外部投影读必须有 AI PG active/coverage 证明，失败回退 PG。
- scope 先由 core 校验、分页后参与召回；reranker/模型之前及返回证据前回验。最多 50 个指定文件，不设每文件最多两片；引用含 source/content/parent/child 版本。普通 RAG 不代表全文穷尽分析。
- `parser-svc` 拉起 Python `parser_worker.server`；保留文本、PDF、DOCX、CSV、XLSX 等现有能力，不宣称 PPTX、旧 DOC/PPT 或 OCR 已完整支持。图片缩略图仍走原 media 链。
- 业务 36 只建立切换标记，不自动移动旧表。`ai-upgrade.sh prepare/verify/archive` 提供保留数据的分批交接、重建和独立归档；旧 computed 无证明时隔离，不丢结果重算，不重跑核心 async-import，不 purge。归档 down 恢复旧表/FK，不反向转换新 AI 数据。
- AI staging、退休版本及未接纳外部物理输出按安全宽限清理；active、未提交结果、墓碑和 receipt 保留。维护任务只由控制库调度。

### 缓存与锁

- drive 的读缓存（进程内 L1 加 Redis）的 key 里带着同一 SQL 快照组合的 `owner_content_epochs.content_epoch + owner_async_epochs.content_epoch` 和 ACL epoch，每次读都先查已提交的版本。跨副本失效靠写路径在业务事务里用 `ownerepoch.BumpContent` / `BumpACL` 推进版本；按 key 删除只是本进程的补充。权限和配额判断始终读事实表。
- 事务内互斥用 PG advisory lock（`platform/dblock`）。新异步模块/扫描租约位于控制 PG；Redis 不参与异步执行权威。配额热行是 `user_quota_usage.occupied_bytes`，身份行不再承接日常占用更新。

### 配置

环境变量在 `platform/config/config.go` / `async.go` 读取，入口调用 `LoadForRole`，按角色校验。`deploy/.env` 是唯一的配置来源，`deploy/compose.yaml` 用 `x-app-environment` 锚点把同一组变量传给各 Go 服务。新增一个变量要改四处：`config.go`、`compose.yaml` 的锚点、`deploy/.env.example`、`deploy/.env.no-ai.example`。两个模板的键必须保持一致。

### 前端

`web/` 是 Create React App 项目（React 18、Ant Design 5、JavaScript）。开发时 `src/setupProxy.js` 把 `/api` 代理到 API、把 `/storage` 代理到 MinIO，和线上 Nginx 的同源布局一致。
