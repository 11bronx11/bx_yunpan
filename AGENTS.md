# AGENTS.md

BX YunPan：React 18 + Go（Gin）的私有云盘，带分片上传、秒传、分享、缩略图和可选的 AI 检索与 RAG 问答。本文件给在这个仓库里工作的 coding agent 用，只写要跨多个文件才能看明白的东西，细节以源码为准。

## 先知道这几件事

- **README 有一部分是旧的。** 任务队列在 2026-10-08 从 Asynq 换成了 RabbitMQ，README 的架构图和「Redis/Asynq Worker」等说法没跟上。当前异步系统已改成独立控制 PG + 明确计算角色 + committer；Redis 只做缓存与限流，不再负责异步选主。当前入口见 `docs/ASYNC_SYSTEM_IMPLEMENTATION_2026-11-16.md`；两个 11-03 文档保留为旧源码快照。
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

- `TEST_POSTGRES_DSN`：专用业务测试库；绝大多数集成用例
- `TEST_CONTROL_POSTGRES_DSN`：专用独立控制测试库
- `TEST_ASYNC_CORE=true` + `TEST_POSTGRES_READONLY_DSN`：开启真实 HTTP/MQ/MinIO 核心冒烟，读角色必须无业务写权限
- `TEST_RABBITMQ_URL` + `TEST_RABBITMQ_MANAGEMENT_URL`
- `TEST_MINIO_ENDPOINT` / `TEST_MINIO_ACCESS_KEY` / `TEST_MINIO_SECRET_KEY`
- `TEST_REDIS_ADDR`
- `TEST_SEARCH_ES_ENDPOINT` / `TEST_SEARCH_QDRANT_ENDPOINT`

夹具的隔离程度不一样：identity、outbox 等包在目标库里建独立 schema（RabbitMQ 用独立 vhost），结束时删除；storageops、recovery、upload 并发、sharing、search 等夹具使用独立迁移 schema；仍有兼容用例读写目标库 `public`，不能据此把整个测试套件指向开发库。outbox 的夹具还用 `CREATE TABLE ... LIKE public.<表>` 克隆表结构。所以要用一个跑过迁移的专用测试库，不要指向开发库。

迁移文件是 `backend/migrations/NNNNNN_name.{up,down}.sql`（golang-migrate，当前到 000034；控制迁移独立放在 `backend/control-migrations/`，当前到 000002）：

```bash
./deploy/local-run.sh migrate up        # 业务库；还支持 down-one、version
./deploy/local-run.sh migrate control up # 独立控制库
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

`local-run.sh` 和 `local-frontend.sh` 从 `deploy/.env` 派生宿主机地址，包括 RabbitMQ、业务只读 PG 和控制 PG；不要另外维护配置。默认宿主机 metrics 端口由角色分配为 18083–18091，可用调用环境中的 `WORKER_METRICS_ADDR` 覆盖。`worker` 只是 `async-control` 的兼容别名，不再启动计算角色。
## 架构

### 进程与部署形态

`backend/` 是一个 Go module。`cmd/*` 的入口装配在 `internal/bootstrap`。业务 PG 是唯一业务事实源；独立控制 PG 是唯一任务执行权威。MinIO 保存字节；Redis、Elasticsearch、Qdrant 是可重建的加速层。控制库可以位于独立 PG 实例，测试使用同一实例的两个数据库不等于实例资源隔离。

核心默认进程是 `api`、`async-control`、`committer`、`upload-worker`、`object-worker`、`media-worker`、`gc-worker`。`--distributed-workers` 现为默认形态的兼容参数。`--ai-split` 仍拆分查询与索引，`--search-indexer` 启用搜索投影。旧 `worker` / `outbox-relay` 命令只映射控制角色，`maintenance-worker` 只映射 GC；不要与对应新角色一起启动。

核心计算进程通过 `dependencies.OpenAsync` 取得控制池、保留控制池及业务只读池，没有业务写池；业务只读角色必须无写权限。`committer` 额外取得业务写池。API 不打开控制 PG 或 broker；其配置校验和 HTTP readiness 不要求二者健康。AI/search 仍有派生数据写连接，这是本轮保留的兼容边界。

### 异步链路与修改入口

1. `outbox.AddTyped` 在事实事务内只追加 `outbox_events`，不写 deliveries，旧 V2/search context 开关不再静默丢弃事实事件。
2. `asynccontrol.Relay` 用同语句快照的 `xmin` 和 `(event_xid,seq)` 索引读取；inbox、冻结计划、任务/来源、位点在控制库同事务接管。不能改回只按自增号推进。
3. 任务契约与预算在 `asynccontrol/model.go`，订阅在 `asyncjobs/planner.go`，计算在 `asyncjobs/compute.go`；共享载荷在 `asyncprotocol`。新增类型要同步这些入口与领域提交函数。
4. 控制库管理有限执行/派发/失租/结果补发预算、owner 公平、背压及模块租约。MQ 拓扑是 `bx.async.v1.work.<queue>`、32 个 `bx.async.v1.result.NN` 和 dead 队列；不使用旧 TTL 重试。结果分片采用 SAC + 成员/模块租约分配。
5. worker 条件领取、续租并保存类型化结果后可 ACK 工作通知；写进程先提交业务效果与 `async_commit_receipts`，再回报控制库，最后 ACK 结果。computed 结果只补发，不重算。
6. 领域提交在 upload/objectstore/media 的 `async_commit.go` 或 `async.go`；`asyncjobs.Committer` 组织事务、小批、配额/epoch 合并及逐条回退。事务内不能调用 MinIO。
7. `storage_operations` 现在保存业务意图/提交结论；旧执行租约与预算只供导入及兼容测试。新启动链不调用旧 dispatcher、taskqueue middleware 或 recovery 巡检。在线业务扫描仅保留 GC 的上传会话到期兜底。
8. `async-import` 在旧写者全部停止后导入保留事件/操作，继承旧预算、成功及停止结论，逐条持久转移/隔离旧 MQ 消息后 ACK，不 purge。`async-admin inspect` 定位任务，`redo` 校验当前前提并创建留痕子任务；不会重开原停止任务。

长期输入、结果和执行详情有界维护；终态语义屏障、停止预算和未提交结果保留。`internal/workflow` 仍没有接入启动链，不要引入第二个任务权威。`docs/ASYNC_CONTROL_PLANE_DESIGN_2026-11-12.md` 是历史提案，当前施工基准是根目录施工书。

### 上传与对象模型

文件字节不经过 Go API：API 只签发预签名 URL，浏览器经同源的 `/storage` 直传 MinIO。逻辑文件（`file_entries`）和物理对象（`file_objects`，按 SHA-256 加大小内容寻址，带 `reference_count`）是分开的。秒传和分享导入都只是新增 entry 并给对象加引用。

创建、完成、中止这几个请求只改 PostgreSQL 状态，对 MinIO 的调用都由 worker 完成，所以一次分片上传是被异步任务一步步推进的：

`Create`（会话 `initializing`，登记 `multipart.create` 操作）→ upload 队列建好 multipart → 客户端传分片并确认 → `Complete`（`completing`，登记 `multipart.complete` 操作）→ `verifying`，发出 `object.verify_requested` → object 队列只校验 SHA-256/大小 → committer 校验前提，必要时登记安全世代复制 → upload 队列复制字节 → committer 建 entry、加引用、结算配额，并发出 `file.created` 和 `object.ready` → media 计算、committer 发布缩略图；AI 角色建索引。

引用归零的对象由 `object.gc_requested` 延迟回收，延迟由 `OBJECT_GC_DELAY` 控制，默认 24h。先由 committer 授权进入 deleting，GC 删除对应世代字节后，再由 committer 提交 deleted。暂存仅对 `staging/` 配过期规则；MinIO 省略 multipart lifecycle 时，使用操作专属 key 的延迟 sweep 和原生 stale-upload 兜底，不给正式 compute 输出设置自动过期。

### AI、解析与搜索

- `AI_ENABLED=false` 时 API 使用 `ai.DisabledBackend`，搜索、问答、重建索引都返回 `503 ai.unavailable`，控制订阅计划记录 AI 未订阅。
- API 调 aisvc 走 etcd 注册、自定义 gRPC resolver（`platform/registry`）和 `round_robin`。Search 和 Ask 走 gRPC；Reprocess 是 core-api 自己的命令，在本地事务里写任务和 outbox。
- `parser-svc` 是 Go 进程，启动时拉起 Python 子进程 `parser_worker.server`（代码在 `backend/parser_worker/`）做文档解析。
- 向量维度在迁移里写死为 `vector(1024)`，`AI_EMBEDDING_DIMENSION` 不一致时 `up.sh` 会拒绝启动。
- 检索默认读 PostgreSQL（FTS、pgvector、RRF 融合）。Elasticsearch 和 Qdrant 是可选的派生投影，由 `search/projector.go` 写入，可以重建，对业务表没有外键。

### 缓存与锁

- drive 的读缓存（进程内 L1 加 Redis）的 key 里带着同一 SQL 快照组合的 `owner_content_epochs.content_epoch + owner_async_epochs.content_epoch` 和 ACL epoch，每次读都先查已提交的版本。跨副本失效靠写路径在业务事务里用 `ownerepoch.BumpContent` / `BumpACL` 推进版本；按 key 删除只是本进程的补充。权限和配额判断始终读事实表。
- 事务内互斥用 PG advisory lock（`platform/dblock`）。新异步模块/扫描/分片租约位于控制 PG；Redis 不参与异步执行权威。配额热行是 `user_quota_usage.occupied_bytes`，身份行不再承接日常占用更新。

### 配置

环境变量在 `platform/config/config.go` / `async.go` 读取，入口调用 `LoadForRole`，按角色校验。`deploy/.env` 是唯一的配置来源，`deploy/compose.yaml` 用 `x-app-environment` 锚点把同一组变量传给各 Go 服务。新增一个变量要改四处：`config.go`、`compose.yaml` 的锚点、`deploy/.env.example`、`deploy/.env.no-ai.example`。两个模板的键必须保持一致。

### 前端

`web/` 是 Create React App 项目（React 18、Ant Design 5、JavaScript）。开发时 `src/setupProxy.js` 把 `/api` 代理到 API、把 `/storage` 代理到 MinIO，和线上 Nginx 的同源布局一致。
