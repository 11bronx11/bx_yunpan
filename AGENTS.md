# AGENTS.md

BX YunPan：React 18 + Go（Gin）的私有云盘，带分片上传、秒传、分享、缩略图和可选的 AI 检索与 RAG 问答。本文件给在这个仓库里工作的 coding agent 用，只写要跨多个文件才能看明白的东西，细节以源码为准。

## 先知道这几件事

- **README 有一部分是旧的。** 任务队列在 2026-10-08 从 Asynq 换成了 RabbitMQ，README 的架构图和「Redis/Asynq Worker」等说法没跟上。Redis 现在只做缓存、限流和 dispatcher 选主。按源码整理的现状文档是 `docs/CURRENT_CODE_FACTS_2026-11-03.md` 和 `docs/PROCESS_STARTUP_AND_FLOW_2026-11-03.md`。
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
make proto           # 改了 api/proto 后重新生成 internal/ai/pb（生成物入库）
make proto-check     # 校验生成物没有漏提交
go generate ./api    # 改了 api/openapi.yaml 后重新生成 internal/contract/api.gen.go
```

HTTP handler 不依赖 `internal/contract` 的生成代码，改接口时要同时手改 `backend/api/openapi.yaml`（它被嵌入二进制，由 API 的 `/docs` 展示）。

集成测试靠环境变量开启，没设就 skip：

- `TEST_POSTGRES_DSN`：绝大多数集成用例
- `TEST_RABBITMQ_URL` + `TEST_RABBITMQ_MANAGEMENT_URL`
- `TEST_MINIO_ENDPOINT` / `TEST_MINIO_ACCESS_KEY` / `TEST_MINIO_SECRET_KEY`
- `TEST_REDIS_ADDR`
- `TEST_SEARCH_ES_ENDPOINT` / `TEST_SEARCH_QDRANT_ENDPOINT`

夹具的隔离程度不一样：identity、outbox 等包在目标库里建独立 schema（RabbitMQ 用独立 vhost），结束时删除；storageops、recovery 等包直接读写目标库 `public` 下的表，只按自己的 key 清理。outbox 的夹具还用 `CREATE TABLE ... LIKE public.<表>` 克隆表结构。所以要用一个跑过迁移的专用测试库，不要指向开发库。

迁移文件是 `backend/migrations/NNNNNN_name.{up,down}.sql`（golang-migrate，当前到 000031）：

```bash
./deploy/local-run.sh migrate up        # 还支持 down-one、version
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
./deploy/init-env.sh [--no-ai]    # 生成 deploy/.env，已存在则不动
./deploy/infra-up.sh              # 只起 postgres、redis、minio
./deploy/local-run.sh migrate up
./deploy/local-run.sh api         # 也可以是 worker、aisvc、parser-svc
./deploy/local-frontend.sh        # :3000
```

`local-run.sh` 和 `local-frontend.sh` 从 `deploy/.env` 派生宿主机地址，不要另外维护一份配置。注意 `infra-up.sh` 不启动 RabbitMQ，`local-env.sh` 也不派生 `RABBITMQ_URL`，而用 `config.Load()` 的业务进程（包括 api）启动时的配置校验都要求它非空。模板 `.env` 里它是空值；变量完全未设置时的默认值 `amqp://guest:guest@127.0.0.1:5672/` 又和 compose 里 RabbitMQ 的默认账号 `yunpan` 对不上。所以宿主机跑业务进程前，要自己用 compose 起 `rabbitmq` 服务，并导出指向 `127.0.0.1:5672` 的 `RABBITMQ_URL`。

## 架构

### 进程与部署形态

`backend/` 是一个 Go module。`cmd/*` 的入口都很薄，真正的装配在 `internal/bootstrap`。业务进程通过 `platform/dependencies.Open` 连同一个 PostgreSQL（一套 GORM 连接池），它是唯一的业务事实源。MinIO 存文件字节，Redis、Elasticsearch、Qdrant 都是可重建的加速层。

这是模块化单体，不是微服务。identity、drive、upload、sharing、objectstore 都在 `api` 进程里，靠函数调用和本地事务协作。只有 AI 有真正的进程边界：`aisvc` 走 gRPC（proto 在 `backend/api/proto/ai/v1`），负责建索引和检索。它和核心服务共用数据库，对 drive 的表只读，检索时把 `owner_id` 和目录范围直接下推到 SQL。

同一份代码有几种进程拓扑，由 `deploy/up.sh` 的参数选择 compose profile：

| 形态 | 进程 |
|---|---|
| 默认 | `worker`（dispatcher、object/media/maintenance/upload 四个队列、周期扫描）和 `aisvc`（gRPC 查询、ai 队列） |
| `--distributed-workers` | `outbox-relay`（只发布，不消费队列），以及各消费一个队列的 `object-worker`、`media-worker`、`maintenance-worker`（含周期扫描）、`upload-worker` |
| `--ai-split` | `ai-query` 和 `ai-indexer`，由 `AI_SERVICE_ROLE` 区分 |
| `--search-indexer` | `search-indexer`，消费 search 队列 |

两代 worker 不能同时消费同一个队列，`up.sh` 切换形态时会停掉另一代。队列 handler 的注册表有三处：`bootstrap/worker.go`（合并形态）、`bootstrap/roles.go`（拆分形态）、`bootstrap/aisvc.go`（ai 队列）。新增或改名任务类型要一起改。

### 异步链路：outbox → RabbitMQ → 执行账本

改 `outbox`、`taskqueue`、`recovery`、`storageops` 之前先读这一节。

1. 业务事务里调 `outbox.AddTyped`，在同一个事务里写一行 `outbox_events`，再按路由表给每个目的地写一行 `outbox_deliveries`。事件契约在 `eventcontract/registry.go` 和 `payloads.go`，路由表在 `outbox/routes.go`（`Routes`、`SearchRoutes`）。
2. `outbox.Dispatcher`（`outbox/dispatcher.go`）靠 Redis 锁选出单个 leader，由 PG `LISTEN/NOTIFY` 唤醒，带退避的轮询兜底。它用 `FOR UPDATE SKIP LOCKED` 加租约 token 领取，逐条发布到 RabbitMQ 并等 confirm，再回写 published。任务 ID 固定为 `<event_id>:<taskType>@<queue>`，重发不变。
3. `taskqueue/taskqueue.go` 声明六个 quorum 队列：`object`、`media`、`maintenance`、`upload`、`ai`、`search`，各带一个 `.dlq` 和一组固定 TTL 的重试队列（1s、5s、30s、1m、5m）。
4. 每个 handler 都被 `taskqueue.Store.Middleware` 包住：先在 `task_executions` 里领取并续租，结束后写 `task_results`，终止失败另写 `task_failures`。重复投递据此短路。
5. 投递语义是 at-least-once。业务幂等靠各 handler 自己的状态机和唯一约束。`event_consumptions`（`outbox.ConsumeOnce`）目前只有 search projector 在用。
6. 对象存储的副作用走 `storageops`：`storage_operations` 行自带租约、fencing token 和 12 次尝试上限，对 MinIO 的调用都在数据库事务之外。
7. `recovery.Reconciler` 周期扫描卡在中间态的业务行，通过 `outbox.EnsureRecovery` 补发确定性事件（记在 `outbox_recoveries`，每个恢复族最多 3 个）。它不直接写 broker。

容易踩的地方：

- **事件会被开关静默丢弃。** `OUTBOX_V2_EVENTS_ENABLED`（默认 false）关着时，`file.updated`、`file.deleted`、`folder.*`、`share.created`、`share.updated`、`share.revoked` 的 `AddTyped` 直接返回 nil。search 目的地只在 `SEARCH_INDEX_ENABLED`（默认 false）打开时才生成。这两个开关放在 `context` 里传递（`outbox.WithV2EventsEnabled`、`outbox.WithSearchEnabled`，在各 bootstrap 入口和 HTTP 路由里设置），新入口忘了设就等于关。
- **同一个 handler 注册了两个名字**：旧的事件类型名和带版本的任务名（见 `outbox.canonicalTaskType`）。这是从 Asynq 迁移留下的兼容窗口。代码里剩下的 `asynq` 字样只用于历史数据和 `cmd/migrate-asynq`。
- `internal/workflow` 有状态机和对应的表，但没有接进任何进程的启动链路。
- 这套链路的重构提案在 `docs/ASYNC_CONTROL_PLANE_DESIGN_2026-11-12.md`，状态是待评审、未实现。

### 上传与对象模型

文件字节不经过 Go API：API 只签发预签名 URL，浏览器经同源的 `/storage` 直传 MinIO。逻辑文件（`file_entries`）和物理对象（`file_objects`，按 SHA-256 加大小内容寻址，带 `reference_count`）是分开的。秒传和分享导入都只是新增 entry 并给对象加引用。

创建、完成、中止这几个请求只改 PostgreSQL 状态，对 MinIO 的调用都由 worker 完成，所以一次分片上传是被异步任务一步步推进的：

`Create`（会话 `initializing`，登记 `multipart.create` 操作）→ upload 队列建好 multipart → 客户端传分片并确认 → `Complete`（`completing`，登记 `multipart.complete` 操作）→ `verifying`，发出 `object.verify_requested` → object 队列校验 SHA-256，在一个事务里建 entry、加引用、结算配额，并发出 `file.created` 和 `object.ready` → media 队列生成缩略图，ai 队列建索引。

引用归零的对象由 `object.gc_requested` 延迟回收，延迟由 `OBJECT_GC_DELAY` 控制，默认 24h。

### AI、解析与搜索

- `AI_ENABLED=false` 时 API 使用 `ai.DisabledBackend`，搜索、问答、重建索引都返回 `503 ai.unavailable`，dispatcher 也不再扇出 ai 任务。
- API 调 aisvc 走 etcd 注册、自定义 gRPC resolver（`platform/registry`）和 `round_robin`。Search 和 Ask 走 gRPC；Reprocess 是 core-api 自己的命令，在本地事务里写任务和 outbox。
- `parser-svc` 是 Go 进程，启动时拉起 Python 子进程 `parser_worker.server`（代码在 `backend/parser_worker/`）做文档解析。
- 向量维度在迁移里写死为 `vector(1024)`，`AI_EMBEDDING_DIMENSION` 不一致时 `up.sh` 会拒绝启动。
- 检索默认读 PostgreSQL（FTS、pgvector、RRF 融合）。Elasticsearch 和 Qdrant 是可选的派生投影，由 `search/projector.go` 写入，可以重建，对业务表没有外键。

### 缓存与锁

- drive 的读缓存（进程内 L1 加 Redis）的 key 里带着 `owner_content_epochs` 表的 `content_epoch` 和 `acl_epoch`，每次读都先查已提交的版本。跨副本失效靠写路径在业务事务里用 `ownerepoch.BumpContent` / `BumpACL` 推进版本；按 key 删除只是本进程的补充。权限和配额判断始终读事实表。
- 事务内互斥用 PG advisory lock（`platform/dblock`）。Redis 锁只用于 dispatcher 选主。

### 配置

所有环境变量在 `platform/config/config.go` 的 `Load()` 里读取，并在启动时由 `Validate()` 校验。`deploy/.env` 是唯一的配置来源，`deploy/compose.yaml` 用 `x-app-environment` 锚点把同一组变量传给各 Go 服务。新增一个变量要改四处：`config.go`、`compose.yaml` 的锚点、`deploy/.env.example`、`deploy/.env.no-ai.example`。两个模板的键必须保持一致。

### 前端

`web/` 是 Create React App 项目（React 18、Ant Design 5、JavaScript）。开发时 `src/setupProxy.js` 把 `/api` 代理到 API、把 `/storage` 代理到 MinIO，和线上 Nginx 的同源布局一致。
