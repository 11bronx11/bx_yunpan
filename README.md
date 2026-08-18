# BX YunPan

BX YunPan 是一个 React 18 + Go Gin 的智能私有云盘。项目以网盘核心链路为主，覆盖目录化文件管理、S3 Multipart 上传、断点恢复、SHA-256 秒传、Share Key 导入、异步缩略图，以及混合检索和有证据约束的 RAG 问答。

**技术栈定位：模块化单体 + AI 服务独立部署。** 后端主体是模块化单体，按 Identity、Drive、Upload、Object、Sharing 等业务边界组织代码，共享一个本地事务边界；只有 AI 模块（文本抽取、OCR/Vision、Embedding）因为 CPU/IO 密集、请求特征与 API 显著不同，被拆成独立进程 `aisvc`，经 gRPC 调用并独立扩容。这不是完整的微服务架构：`ai_*` 表由 aisvc 独占写入，但 drive 的 `file_entries` / `folders` / `file_objects` 保持只读共享，以保留把权限过滤下推到 SQL 的检索设计。取舍理由见「架构取舍」。

## 核心能力

- Ed25519 JWT Access Token、HttpOnly Refresh Cookie、Refresh 轮换和复用检测。
- 用户目录树、面包屑、文件与目录移动、重命名、空目录安全删除和乐观锁版本控制。
- MinIO/S3 Multipart 预签名直传、分片确认、暂停恢复、强 SHA-256 校验、配额预占和过期会话回收。
- FileEntry 与 FileObject 分离，使用 SHA-256 + 文件大小进行内容寻址；同一用户全盘内容唯一，跨用户上传不暴露去重命中，授权 Share Key 可直接复用对象。
- 图片缩略图、对象延迟 GC、Transactional Outbox 和 Redis/Asynq Worker。
- 可选 AI 模式支持 TXT、Markdown、常见源码/配置文件、PDF、DOCX 文本抽取，以及 JPG/PNG 的可插拔 OCR/Vision Provider；二进制 MIME 的源码可按文件名安全回退，并受控兼容 UTF-8 与 GB18030 文本。
- 可选 AI 模式支持文件名精确、前缀和模糊匹配分级排序，以及 PostgreSQL FTS + pgvector + Reciprocal Rank Fusion 混合检索。
- 可选 AI 模式的 RAG 最多选取 8 个证据片段、每文件最多 2 个；模型结构化返回 Grounded 判断和证据 ID，后端再按授权证据白名单校验 Citation。
- 证据不足时保留模型回答但不附带无关引用；文件、段落和页码 Citation 始终受当前用户文件权限约束。上述 AI 能力仅在 `AI_ENABLED=true` 时可用。
- 结构化日志、Request ID、Prometheus 指标、存活和依赖就绪探针。

### 文件能力边界

| 能力 | 支持范围 |
| --- | --- |
| 上传与下载 | 任意非空文件，受用户剩余配额限制 |
| 在线图片预览 | JPG、PNG、GIF、WebP |
| AI 索引（需启用 AI） | TXT、Markdown、JSON、CSV、常见源码/配置文件、PDF、DOCX、JPG、PNG |
| AI 摘要（需启用 AI） | 已成功建立 AI 索引的文档和源码文件 |

同一目录不允许存在两个同名文件，即使内容不同也会返回 `upload.name_conflict`。同一用户上传不同名称但内容相同的文件时，不会创建重复条目，并返回 `upload.file_exists`。

## 架构

```text
Browser
   |
Nginx + React
   |-- /api/* ----------> Go Gin API
   |                       |  |
   |                       |  +-- gRPC + etcd resolver --> aisvc [可选 AI profile]
   |                       |                              |-- DashScope / fake AI
   |                       |                              |-- Redis ai queue
   |                       |                              +-- PostgreSQL (shared read)
   |                       |
   |                       +--> PostgreSQL + pgvector
   |                       +--> Redis + Asynq
   |                       +--> MinIO presign
   |-- /storage/* -------> MinIO/S3
                           ^
                    Go Worker
             (verify/media/outbox/GC)

 API / Worker / aisvc --OTLP trace--> Collector --> Jaeger
 Prometheus --------scrape /metrics---------> API
```

API 负责鉴权、元数据和预签名 URL，文件字节不经过 Go API。PostgreSQL 是业务事实源，Outbox 在业务事务中记录异步事件；Worker 独立完成对象校验、缩略图、Outbox 投递和 GC。启用 AI 时，aisvc 独立消费 AI 队列并承载检索、问答和索引。

API 保持无状态，文件内容使用对象存储承载，异步任务通过版本化事件解耦。AI 模式下，aisvc 通过 etcd lease 注册，API 使用 gRPC resolver + round_robin 发现实例；Worker 多副本由 Redis 锁保证 Outbox dispatcher 单主投递。`AI_ENABLED=false` 时只运行核心网盘拓扑，API 使用禁用后端，aisvc 和 etcd 不启动。

### 架构取舍

- **只拆 AI，不做全面微服务化。** 抽取、OCR/Vision 和 Embedding 是 CPU/IO 密集任务，有独立扩容和资源隔离的真实收益；Identity、Drive、Upload、Sharing 仍需要本地事务，不拆分就不引入分布式事务。
- **共享数据库，但分清读写所有权。** aisvc 独占写入 `ai_documents` / `ai_chunks`，只读访问 `file_entries` / `folders` / `file_objects`。检索的 `owner_id` 和目录范围在 SQL 中过滤，避免先做全局向量召回再在应用层丢弃越权结果。
- **服务发现故障降级。** etcd 不可用时 resolver 保留最后一次地址列表继续服务；这牺牲了新实例立即发现和下线实例立即摘除，换取注册中心故障不直接导致 AI 全面不可用。
- **锁按边界选择。** 事务内的同名写入使用 PostgreSQL advisory lock；跨多轮 Outbox 轮询的 dispatcher 选主使用带 token 校验和续期的 Redis 锁。两者不是互相替换关系。

### 本轮实测

在 4 vCPU、5.2 GiB 内存、Docker Compose、`AI_PROVIDER=fake`、52 个 active 文件和 629 个 AI chunk 的本机环境中，已实测整栈健康、上传→索引→检索→问答、Jaeger 跨 api/worker/aisvc trace，以及 `aisvc=2` 的均衡和实例摘除。性能数字和限制见 [`/benchmark.md`](/benchmark.md)，上传 trace 截图见 [`/jaeger-upload-trace.png`](/jaeger-upload-trace.png)。这些是小数据集 smoke 基线，不是生产容量承诺。

![Jaeger 中跨 api、worker、aisvc 的上传索引链路](jaeger-upload-trace.png)

## Docker 一键启动

### 环境要求

- Docker Engine 24+
- Docker Compose v2
- 首次构建建议至少保留 `6GB` 磁盘空间
- 支持 `amd64` 和 `arm64`

### 从 GitHub 拉取后运行

直接体验完整网盘功能：

```bash
git clone https://github.com/11bronx11/bx_yunpan.git
cd bx_yunpan
./deploy/up.sh
```

首次执行时，`up.sh` 会自动生成 `deploy/.env`、构建业务镜像并启动全部服务。默认使用 `AI_PROVIDER=fake`，不需要外部密钥，适合先验证上传、目录、分享和检索流程。启动完成后访问 `http://127.0.0.1:3000`，再执行下面的命令确认所有链路正常：

```bash
./deploy/status.sh
```

只需要目录、上传下载、分享、缩略图与对象回收，不启动 aisvc 和 etcd：

```bash
./deploy/init-env.sh --no-ai
./deploy/up.sh
./deploy/status.sh
```

该模式使用完整的 [`deploy/.env.no-ai.example`](deploy/.env.no-ai.example) 模板：搜索、问答和重建索引接口立即返回 `503 ai.unavailable`，新上传不会产生 AI 索引任务，已有文件、数据库记录、对象和历史 AI 数据不会被删除。完整模板与无 AI 模板包含相同的配置键，便于以后重新启用 AI。

已有 `deploy/.env` 不会被 `init-env.sh` 覆盖。将现有部署切换到无 AI 模式时，修改其中四项：

```dotenv
AI_ENABLED=false
AI_PROVIDER=disabled
AISVC_REPLICAS=0
ETCD_ENABLED=false
```

升级到首次包含无 AI 支持的代码版本后，执行一次完整的 `./deploy/up.sh`，确保 API 和 Worker 镜像包含禁用逻辑。以后仅切换这组配置且镜像未变时可以使用 `./deploy/up.sh --no-build`。脚本会停止并移除 aisvc/etcd 容器，但保留数据库、对象存储、历史 AI 数据和 Docker 数据卷。

需要使用真实 AI 索引、语义检索和 RAG 问答时，建议在第一次启动前完成配置：

```bash
git clone https://github.com/11bronx11/bx_yunpan.git
cd bx_yunpan
./deploy/init-env.sh
```

编辑 `deploy/.env`，至少修改以下两项，不要把该文件或密钥提交到 Git：

```env
AI_PROVIDER=dashscope
DASHSCOPE_API_KEY=你的阿里云百炼 API Key
```

然后执行：

```bash
./deploy/up.sh
./deploy/status.sh
```

脚本会自动执行以下步骤：

1. 生成不进入 Git 的 `deploy/.env`，全新部署使用随机密码和签名密钥。
2. 检查 Docker、Compose、AI Provider 配置和剩余磁盘空间。
3. 构建 Web、API、Worker、Migrate 镜像；仅在 AI 开启时构建 aisvc。
4. 启动 PostgreSQL、Redis、MinIO，并在 AI 开启时启动 etcd；创建私有 Bucket。
5. 执行数据库迁移。
6. 启动 API、Worker、Web，并在 AI 开启时启动 aisvc；等待健康检查通过。

本机空间低于 `6GB` 时脚本会拒绝构建，避免中途写满磁盘。首次启动或者拉取了新的 Go、React、Dockerfile、Compose、Nginx 代码后，应执行完整的 `./deploy/up.sh`。

`up.sh` 默认通过 `deploy/build.sh` 逐个构建镜像；单个构建限制为 1 CPU、1.5 GiB 内存和 2 GiB 内存加 Swap，并在开始下一个镜像前检查宿主机可用内存。不要在小内存主机上并行执行多个 `docker compose build`。

只有业务镜像已经存在，并且仅修改了 `deploy/.env` 配置时，才使用下面的命令跳过构建：

```bash
./deploy/up.sh --no-build
```

`--no-build` 仍会根据 `deploy/.env` 重建需要更新配置的容器，但不会重新编译源码；全新环境不能使用该参数。

同时启动 Prometheus、Grafana、OpenTelemetry Collector 和 Jaeger。需要实际导出应用 trace 时，先在 `deploy/.env` 中设置 `OTEL_TRACES_ENABLED=true`：

```bash
./deploy/up.sh --observability
```

Prometheus 当前直接抓取 API 的 `/metrics`；应用 trace 由 API、Worker 和 aisvc 经 Collector 写入 Jaeger。`OTEL_TRACES_ENABLED` 默认为 `false`，修改后需要通过 `up.sh` 重建相关容器配置。

AI 开启时可按需扩容 aisvc；该参数会覆盖 `deploy/.env` 中的 `AISVC_REPLICAS`：

```bash
./deploy/up.sh --no-build --aisvc-replicas 2
./deploy/status.sh
```

### 服务入口

| 服务 | 默认地址 | 用途 |
| --- | --- | --- |
| Web | `http://127.0.0.1:3000` | 云盘界面和同源对象上传 |
| API | `http://127.0.0.1:8081` | API、健康探针和 OpenAPI |
| API 文档 | `http://127.0.0.1:8081/docs` | Redoc 接口文档 |
| MinIO Console | `http://127.0.0.1:9001` | 对象存储管理 |
| Prometheus | `http://127.0.0.1:9090` | 可选指标查询 |
| Grafana | `http://127.0.0.1:3001` | 可选监控面板 |
| Jaeger | `http://127.0.0.1:16686` | 可选分布式 Trace 查询 |

### 运维命令

```bash
# 查看容器状态，并检查 Web、API 和对象存储代理链路
./deploy/status.sh

# 单独检查运行容器是否与 deploy/.env 一致
./deploy/config-check.sh

# 跟踪当前模式下的 API、Worker、Web、迁移日志，以及可选的 aisvc、etcd 日志
./deploy/logs.sh

# 只查看指定服务
./deploy/logs.sh api worker

# 停止服务，保留数据库和对象存储数据
./deploy/down.sh
```

只有确认不再需要任何网盘数据时，才使用下面的命令删除数据卷：

```bash
./deploy/down.sh --volumes
```

## 本地开发

需要频繁修改代码时，可以只用 Docker 运行基础设施，Go 和 React 直接在宿主机启动。

要求 Go 1.25.13+、Node.js 22+ 和 Docker Compose v2。

```bash
# 完整 AI 模式生成 deploy/.env；无 AI 开发改用 ./deploy/init-env.sh --no-ai
./deploy/init-env.sh

# 1. 只启动基础设施，业务进程在宿主机运行
./deploy/infra-up.sh

# 2. 使用同一份配置执行数据库迁移
./deploy/local-run.sh migrate up

# 3. API
./deploy/local-run.sh api
```

另开终端启动 Worker：

```bash
./deploy/local-run.sh worker
```

`AI_ENABLED=true` 时再启动 aisvc；`./deploy/init-env.sh --no-ai` 生成的核心网盘模式跳过此步：

```bash
./deploy/local-run.sh aisvc
```

再启动前端：

```bash
./deploy/local-frontend.sh
```

开发服务器访问 `http://127.0.0.1:3000`。前端将 `/api` 代理到 `:8081`，将 `/storage` 代理到 MinIO `:9000`。
`local-run.sh` 和 `local-frontend.sh` 会从 `deploy/.env` 派生宿主机地址；不要再为宿主机单独维护一份业务配置。宿主机 API/Worker 与 Docker 业务服务不能同时占用相同端口。

## 配置

Docker Compose 和宿主机开发脚本都从 `deploy/.env` 读取配置。不要提交该文件。完整 AI 配置参考 [deploy/.env.example](deploy/.env.example)，核心网盘配置参考 [deploy/.env.no-ai.example](deploy/.env.no-ai.example)；两个模板包含相同的配置键，仅默认运行模式不同。`init-env.sh` 只在 `deploy/.env` 不存在时从所选模板生成配置并写入随机密钥，不会覆盖或合并已有文件。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `APP_ENV` | `development` | 使用 `production` 时必须替换签名密钥 |
| `DOCKER_REGISTRY` | `docker.io` | Docker 基础镜像仓库；网络受限时可切换镜像代理 |
| `GO_PROXY` | `https://proxy.golang.org,direct` | Go 模块下载代理；网络受限时可切换为可达镜像 |
| `APP_BIND_IP` | `127.0.0.1` | Web/API 监听地址 |
| `WEB_PORT` | `3000` | Web 对外端口 |
| `API_PORT` | `8081` | API 对外端口 |
| `HTTP_MAX_BODY_BYTES` | `1048576` | API 请求体全局上限，AI 接口仍使用更严格上限 |
| `HTTP_MAX_HEADER_BYTES` | `65536` | HTTP 请求头总大小上限 |
| `HTTP_SHUTDOWN_TIMEOUT` | `10s` | API 优雅停机等待时间 |
| `HTTP_PROBE_TIMEOUT` | `2s` | 健康检查依赖探测超时 |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | HTTP 请求头读取超时 |
| `HTTP_IDLE_TIMEOUT` | `60s` | HTTP Keep-Alive 空闲超时 |
| `POSTGRES_MAX_OPEN_CONNS` | `32` | PostgreSQL 最大连接数 |
| `POSTGRES_MAX_IDLE_CONNS` | `8` | PostgreSQL 最大空闲连接数 |
| `POSTGRES_CONN_MAX_LIFETIME` | `30m` | PostgreSQL 连接最大生命周期 |
| `POSTGRES_CONN_MAX_IDLE_TIME` | `5m` | PostgreSQL 空闲连接回收时间 |
| `REDIS_DB` | `0` | Redis 逻辑数据库编号 |
| `S3_PUBLIC_ENDPOINT` | `127.0.0.1:3000` | 浏览器实际访问到的 Web Host，参与 SigV4 签名 |
| `S3_PUBLIC_PATH_PREFIX` | `/storage` | Web 代理对象存储的路径前缀 |
| `S3_REGION` | `us-east-1` | S3 签名区域 |
| `S3_SECURE` | `false` | API 到 MinIO 是否使用 HTTPS |
| `S3_READ_URL_TTL` | `10m` | 下载和预览 URL 有效时间 |
| `S3_PUBLIC_SECURE` | `false` | 浏览器预签名 URL 是否使用 HTTPS |
| `AUTH_ISSUER` | `bx-yunpan` | JWT 签发者 |
| `AUTH_SIGNING_SEED` | 自动生成 | Access Token 签名种子 |
| `AUTH_ACCESS_TTL` | `15m` | Access Token 有效期 |
| `AUTH_REFRESH_TTL` | `720h` | Refresh Token 有效期 |
| `AUTH_COOKIE_SECURE` | `false` | HTTPS 部署时设为 `true` |
| `AUTH_COOKIE_DOMAIN` | 空 | Refresh Cookie 的域，单域部署保持为空 |
| `USER_DEFAULT_QUOTA_BYTES` | `10737418240` | 新用户默认网盘容量 |
| `UPLOAD_SESSION_TTL` | `24h` | 未完成上传会话有效期 |
| `UPLOAD_PART_URL_TTL` | `15m` | 分片上传 URL 有效时间 |
| `UPLOAD_CLEANUP_INTERVAL` | `5m` | Worker 扫描过期上传的周期 |
| `UPLOAD_CLEANUP_BATCH` | `100` | 单次过期上传回收数量 |
| `OUTBOX_POLL_INTERVAL` | `1s` | Outbox 事件轮询周期 |
| `OUTBOX_BATCH_SIZE` | `20` | 单批事件投递数量 |
| `OUTBOX_TASK_MAX_RETRY` | `8` | 异步任务最大重试次数 |
| `OUTBOX_TASK_TIMEOUT` | `30m` | 单个异步任务超时 |
| `OBJECT_GC_DELAY` | `24h` | 零引用对象延迟删除时间 |
| `SHARE_SECRET` | 自动生成 | Share Access Token 签名密钥 |
| `SHARE_ACCESS_TTL` | `15m` | Share Access Token 有效期 |
| `WORKER_CONCURRENCY` | `8` | 每个 Worker 或 aisvc 进程的 Asynq 并发数；增加 aisvc 副本会成倍增加 AI 任务总并发上限 |
| `WORKER_QUEUE_AI` | `4` | AI 队列权重 |
| `WORKER_QUEUE_MEDIA` | `4` | 缩略图/媒体队列权重 |
| `WORKER_QUEUE_OBJECT` | `2` | 对象校验队列权重 |
| `WORKER_QUEUE_MAINTENANCE` | `1` | 清理维护队列权重 |
| `AI_ENABLED` | `true` | 是否构建、启动和调用 aisvc；关闭后不投递 AI 索引任务 |
| `AI_PROVIDER` | `fake` | AI 开启时为 `fake` 或 `dashscope`；无 AI 模板使用 `disabled` |
| `DASHSCOPE_API_KEY` | 空 | DashScope 模式必填 |
| `AI_BASE_URL` | DashScope 兼容地址 | OpenAI 兼容 API 地址 |
| `AI_CHAT_MODEL` | `qwen-plus` | 问答模型 |
| `AI_EMBEDDING_MODEL` | `text-embedding-v4` | 向量模型 |
| `AI_VISION_MODEL` | `qwen-vl-plus` | 图片理解/OCR 模型 |
| `AI_EMBEDDING_DIMENSION` | `1024` | 向量维度，数据库列固定为 `vector(1024)` |
| `AI_MAX_OBJECT_MIB` | `32` | AI 抽取单文件大小上限 |
| `AI_REQUEST_TIMEOUT` | `90s` | 单次 AI Provider HTTP 请求超时 |
| `AI_RATE_LIMIT_ENABLED` | `true` | 是否启用 AI 接口的用户级 Redis 限流 |
| `AI_RATE_LIMIT_SEARCH_PER_MINUTE` | `30` | 每个用户每分钟搜索次数 |
| `AI_RATE_LIMIT_ASK_PER_MINUTE` | `10` | 每个用户每分钟 AI 问答次数 |
| `AI_RATE_LIMIT_REPROCESS_PER_MINUTE` | `3` | 每个用户每分钟重建索引次数 |
| `AISVC_REPLICAS` | `1` | `up.sh` 启动的 aisvc 副本数；AI 开启时必须为正整数，仅 `AI_ENABLED=false` 时可设为 `0` |
| `GO_BUILD_PARALLELISM` | `1` | Docker 内 `go build -p`，限制同时编译的包数 |
| `GO_BUILD_MAX_PROCS` | `2` | Docker 内 Go 编译器可使用的最大 CPU 线程数 |
| `COMPOSE_PARALLEL_LIMIT` | `1` | `up.sh` 同时执行的 Compose 构建/启动任务数 |
| `BUILD_MEMORY_LIMIT` | `1536m` | 单个镜像构建容器的内存上限 |
| `BUILD_MEMORY_SWAP_LIMIT` | `2048m` | 单个镜像构建容器的内存与 Swap 总上限 |
| `BUILD_CPU_QUOTA` | `100000` | 配合默认 period 将构建限制为 1 个 CPU 核 |
| `BUILD_MIN_AVAILABLE_MB` | `1536` | 开始下一个镜像前要求的宿主机可用内存 |
| `YUNPAN_MIN_FREE_GB` | `6` | 构建前要求的磁盘最小剩余空间 |
| `YUNPAN_UP_ATTEMPTS` | `3` | Compose 启动失败时的最大尝试次数 |
| `AISVC_GRPC_TARGET` | `aisvc:8082` | API 直连目标；启用 etcd 后改由 resolver 发现 |
| `AISVC_CALL_TIMEOUT` | `120s` | API→aisvc 总预算，不得短于 `AI_REQUEST_TIMEOUT` |
| `ETCD_ENABLED` | `true` | 是否启用 aisvc 注册发现 |
| `ETCD_IMAGE` | `quay.io/coreos/etcd:v3.6.6` | 内置 etcd 镜像 |
| `ETCD_ENDPOINTS` | `etcd:2379` | etcd 客户端地址列表 |
| `ETCD_USERNAME` | 空 | 外部 etcd 用户名；内置 etcd 不启用认证 |
| `ETCD_PASSWORD` | 空 | 外部 etcd 密码；内置 etcd 不启用认证 |
| `ETCD_LEASE_TTL` | `10s` | aisvc 注册租约时长 |
| `OUTBOX_LEADER_LOCK_ENABLED` | `true` | 是否启用 dispatcher Redis 选主 |
| `OUTBOX_LEADER_LOCK_TTL` | `15s` | dispatcher 锁 TTL，由 watchdog 续期 |
| `OTEL_TRACES_ENABLED` | `false` | 是否导出应用 trace |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `otel-collector:4317` | OTLP gRPC Collector 地址 |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | ParentBased 下的根 trace 采样率 |

对象存储有两个端点：

- `S3_ENDPOINT=minio:9000`：API 和 Worker在 Compose 网络内访问。
- `S3_PUBLIC_ENDPOINT=127.0.0.1:3000`：浏览器通过 Nginx `/storage` 同源访问。

部署到域名和 HTTPS 后，需要同步设置：

```dotenv
APP_ENV=production
APP_BIND_IP=0.0.0.0
S3_PUBLIC_ENDPOINT=drive.example.com
S3_PUBLIC_SECURE=true
AUTH_COOKIE_SECURE=true
```

TLS 建议由宿主机 Caddy、Nginx 或云负载均衡终止，不要直接暴露 PostgreSQL、Redis 和 MinIO 数据端口。

### 远程开发端口转发

服务器保持 `127.0.0.1` 绑定时，可在 Mac 上执行：

```bash
ssh -N \
  -L 3000:127.0.0.1:3000 \
  -L 8081:127.0.0.1:8081 \
  -L 9001:127.0.0.1:9001 \
  user@server
```

浏览器仍访问 `http://127.0.0.1:3000`，预签名 URL 的 Host 与 `S3_PUBLIC_ENDPOINT` 保持一致。

## AI 模式

项目提供三种运行方式：

- `AI_ENABLED=false`：核心网盘模式，不构建或启动 aisvc/etcd，不产生 AI 队列任务。
- `AI_ENABLED=true + AI_PROVIDER=fake`：默认离线 AI 模式，使用确定性本地 Embedding 和摘要，不依赖外部 Key，适合演示和自动化测试。
- `AI_ENABLED=true + AI_PROVIDER=dashscope`：真实模型模式，需要 DashScope API Key。

文件完成异步索引后，文件名搜索会优先返回精确和前缀匹配；混合检索使用全文排名与向量排名做 RRF 融合。问答阶段从授权结果中选取最多 8 个片段，并限制每个文件最多 2 个，避免单一文档占满上下文。DashScope 以 JSON 返回回答、Grounded 判断和证据 ID，后端只接受本次检索证据集合中的 ID；证据不足或引用无效时，响应中的 `citations` 为空。

启用 DashScope：

```dotenv
AI_PROVIDER=dashscope
DASHSCOPE_API_KEY=sk-...
AI_CHAT_MODEL=qwen-plus
AI_EMBEDDING_MODEL=text-embedding-v4
AI_VISION_MODEL=qwen-vl-plus
AI_REQUEST_TIMEOUT=90s
AI_RATE_LIMIT_ENABLED=true
AI_RATE_LIMIT_SEARCH_PER_MINUTE=30
AI_RATE_LIMIT_ASK_PER_MINUTE=10
AI_RATE_LIMIT_REPROCESS_PER_MINUTE=3
```

修改 `deploy/.env` 后：

- Docker：仅修改 Provider、限流等配置且所需镜像已存在时，执行 `./deploy/up.sh --no-build` 重建容器配置；从无 AI 部署首次启用 AI 时执行完整的 `./deploy/up.sh`，确保 aisvc 镜像存在。
- 宿主机开发：重新启动 `local-run.sh` 或 `local-frontend.sh` 进程。

首次使用 Docker 或宿主机开发时执行 `./deploy/init-env.sh`，脚本会从 `.env.example` 生成带随机密钥的 `deploy/.env`。无 AI 部署执行 `./deploy/init-env.sh --no-ai`，改用 `.env.no-ai.example`。两个模板的配置键保持一致；模板只在首次生成时使用，已有 `deploy/.env` 不会自动被模板覆盖或合并。

启动脚本会在构建镜像前检查 Provider 配置：`AI_PROVIDER=dashscope` 必须提供 `DASHSCOPE_API_KEY`，且 `AI_EMBEDDING_DIMENSION` 必须保持为 `1024`。检查过程不会输出密钥内容。

## 测试与质量检查

```bash
# Go
cd backend
go test -p=1 ./...
go vet ./...

# React
cd ../web
npm run lint
CI=true npm test -- --runInBand
npm run build

# 完整 AI 模式的 Compose 静态展开，不构建镜像
cd ..
docker compose --env-file deploy/.env.example \
  -f deploy/compose.yaml \
  --profile app --profile ai config --quiet

# 无 AI 模式的 Compose 静态展开，不构建镜像
docker compose --env-file deploy/.env.no-ai.example \
  -f deploy/compose.yaml \
  --profile app config --quiet

# 后端热路径快速测试（需要 TEST_POSTGRES_DSN 才会执行真实数据库并发用例）
cd backend
TEST_POSTGRES_DSN='postgres://yunpan:yunpan@127.0.0.1:5432/yunpan?sslmode=disable' go test -count=1 \
  ./internal/platform/config ./internal/platform/httpapi \
  ./internal/drive ./internal/upload ./internal/sharing ./internal/ai

# AI Provider 结构化回答、Grounded 判断和 Citation 白名单契约
go test ./internal/ai -run 'Test(ParseAnswerResult|DashScopeAnswer)'
```

当前轻量验收覆盖注册鉴权、目录、Multipart 上传、下载、秒传、分享导入、AI 检索、权限隔离、多用户并发读和幂等写，也覆盖 AI 禁用配置、禁用后端的 `503` 契约和 Outbox 跳过 AI fanout。它证明核心链路及模式切换的代码契约可用，但不替代多机容灾、长时间容量测试和真实云环境演练。

## 关键目录

```text
backend/cmd/                  API、Worker、aisvc、Migrate 入口
backend/internal/identity    用户、密码、JWT 和 Refresh Session
backend/internal/drive       目录与逻辑文件
backend/internal/upload      Multipart、断点恢复、校验与配额
backend/internal/objectstore 内容寻址对象、引用计数与 GC
backend/internal/sharing     Share Key 与授权导入
backend/internal/media       图片缩略图
backend/internal/ai          抽取、Embedding、RRF 与 RAG
backend/internal/outbox      Transactional Outbox
backend/migrations           PostgreSQL/pgvector Schema
web/src                      React 云盘工作台
deploy                       Compose、Nginx、监控和部署脚本
```
