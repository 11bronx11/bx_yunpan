# BX YunPan

React 18 + Go（Gin）的私有云盘：目录、分片直传、断点续传、SHA-256 秒传、分享、缩略图，以及可选的全文/语义检索和 RAG 问答。

当前源码入口是 [AI 实现与验证说明](docs/AI_SYSTEM_IMPLEMENTATION_2026-11-16.md) 和 [异步实现说明](docs/ASYNC_SYSTEM_IMPLEMENTATION_2026-11-16.md)。根目录施工书描述目标；实现说明区分源码、测试与实际部署。历史性能报告不能作为当前版本的容量结论。

## 当前架构

```text
浏览器 -- /api --> API -- gRPC + etcd --> ai-query -- core RPC --> core-internal
   |               |                       |                        |
   +-- /storage --> MinIO                AI PG（只读）           业务 PG（只读）
                   ^                       ^
业务 PG / outbox --> async-control --> 控制 PG + RabbitMQ
                          |               |
                    核心计算角色       AI 计算角色 / 独立 ES、QD projector
                          |               |
                       committer       ai-committer
                          |               |
                       业务 PG           AI PG
```

业务 PG 保存用户、目录、文件、权限、配额、对象世代、用户作业及意图；控制 PG 保存唯一任务权威、预算、租约和接纳结果；AI PG 保存规范源块、父子文本、向量、概览和发布版本。AI 在线角色没有业务 SQL 连接。Elasticsearch、Qdrant、Redis 都是可重建的加速层。Redis 不承担异步调度或选主。

上传字节通过同源 `/storage` 直接进入 MinIO。计算角色不写业务事实，业务 committer 在本地事务中提交；AI 计算写 staging，结果经控制库接纳后由 AI committer 发布，receipt 支持重放，后继在确认事务中原子释放。事务内不调用 MinIO 或外部索引。

全文准备、向量、概览、ES 和 Qdrant 独立执行。文本就绪即可全文检索和问答；向量/概览失败不挡文本。全文请求不调用 embedding，外部投影不可用或版本不完整时回退 AI PG。文件名搜索由 core 处理，AI 关闭时仍可用。

问答支持全盘、单文件和最多 50 个指定文件。权限范围先于召回，父子片按 token 预算组装；reranker 和回答使用证据前回验权限，返回引用前再次回验。引用带真实 source/content/parent/child 版本。普通 RAG 的相关片段召回不代表全文穷尽分析；概览使用完整规范源块分段归并。

支持现有文本、代码/配置、文本 PDF、DOCX、CSV、XLSX 解析。PPTX 完整解析、旧 DOC/PPT、扫描件 OCR 不属于已实现的文本能力。图片沿用独立缩略图链路；不支持在线预览的文件可查看信息并下载。

## 启动

要求 Docker Compose v2；宿主机开发要求 Go 1.25.13+、Node.js 22+。受限主机使用串行构建，后端可设 `GOMAXPROCS=2` 和 `go build -p=1`。

```bash
./deploy/init-env.sh              # 已有 deploy/.env 只补缺键，保留现值
./deploy/up.sh                    # 默认显式核心角色和独立 AI 角色
./deploy/status.sh
./deploy/config-check.sh
```

全新无 AI 安装使用 `./deploy/init-env.sh --no-ai`。已有配置切换时编辑 `deploy/.env` 中的 `AI_ENABLED=false`、`AI_PROVIDER=disabled`，再运行 `up.sh`。AI 内容搜索、问答和重建返回 `503 ai.unavailable`；文件名搜索、登录、列表、上传下载、分享和配额不依赖 AI。

配置唯一来源是 `deploy/.env`，不要提交。两个模板的键保持一致：[`deploy/.env.example`](deploy/.env.example)、[`deploy/.env.no-ai.example`](deploy/.env.no-ai.example)。默认 fake provider 可验证链路；真实 provider 的凭据仅由需要模型的角色使用。`AI_VECTORS_ENABLED`、`AI_SUMMARY_ENABLED`、`AI_ASK_ENABLED` 可独立关闭；全文准备不要求模型凭据。

AI 默认角色为 `core-internal`、`ai-query`、`ai-content-worker`、`ai-embedding-worker`、`ai-summary-worker`、`ai-committer` 和 `parser-svc`。核心角色为 `async-control`、`committer`、`upload-worker`、`object-worker`、`media-worker`、`gc-worker`。`--ai-split` 和 `--distributed-workers` 是兼容参数。`aisvc combined` 已退出；`--aisvc-replicas N` 只扩容 `ai-query`。

```bash
./deploy/up.sh --search-backends --search-indexer # 独立 ES/QD 投影
./deploy/up.sh --no-build                        # 使用已构建镜像
./deploy/up.sh --observability
./deploy/logs.sh api ai-query ai-committer
./deploy/down.sh                                # 保留数据卷
```

只配置 ES 或 Qdrant endpoint 时可单独启动对应 projector。投影 epoch 分别由 `AI_TEXT_INDEX_EPOCH`、`AI_VECTOR_INDEX_EPOCH` 控制。`AI_PROJECTION_NAMESPACE` 用于隔离目标，不能在多个环境间共用目标名。每个已发布版本的覆盖证明决定是否切换，AI PG 始终参与版本校验。

默认 Web 为 `http://127.0.0.1:3000`；API 由 Web 代理，`/docs` 提供随二进制发布的 OpenAPI。MinIO Console 为 `:9001`，RabbitMQ 管理界面为 `:15672`。AI gRPC 与数据库仅供内部访问。部署脚本不会自动归档旧表，也不删除旧数据或 purge 队列。

## 保留数据升级

业务迁移到 `000036`，控制迁移到 `000004`，AI 独立迁移到 `000007`；归档使用独立迁移 lane。AI 可使用独立 PG 实例；开发/验收同实例多数据库只证明连接与权限隔离。

先停止旧 AI/search 消费者，构建新角色镜像。只升级 AI 时使用专用工具，不重跑核心 `async-import`：

```bash
AI_LEGACY_WRITERS_STOPPED=true ./deploy/ai-upgrade.sh prepare
./deploy/ai-upgrade.sh inspect
./deploy/ai-upgrade.sh verify
# 历史文本重建完成、旧结果审阅完成后，使用 prepare 输出的 RUN_UUID：
AI_LEGACY_WRITERS_STOPPED=true ./deploy/ai-upgrade.sh archive RUN_UUID
```

prepare 依次 expand、分批建 manifest、旧任务/消息交接、登记重建意图。重复执行继续已有 run。旧 attempts/dispatches/recoveries/deadline 和停止结论保留；无法证明生效的 computed 结果隔离等待审阅。已有新意图通过持久化观察关系接续旧用户作业，避免修改冻结任务和重置预算。verify 未有进展会退出，待任务完成后重跑。

归档前工具检查 manifest、旧任务和消息检查点，写切换标记后才移动旧表到归档 schema；正文、旧 embedding 和 FK 定义保留。`migrate archive down-one` 恢复旧表位置及 FK，但不会反向生成旧格式的新结果。服务回滚需停止新发布；旧 query 只能有限降级读取旧数据，新文件/结果可能缺失。不要同时运行新旧写者。

## 宿主机开发

```bash
./deploy/init-env.sh
./deploy/infra-up.sh
./deploy/local-run.sh migrate up
./deploy/local-run.sh migrate control up
./deploy/local-run.sh migrate ai up
./deploy/local-run.sh reader-init
./deploy/local-run.sh ai-reader-init
```

每个进程在独立终端运行 `./deploy/local-run.sh ROLE`，角色见上文；前端运行 `./deploy/local-frontend.sh`。所有宿主机端点从同一份 `.env` 派生，metrics 端口按角色分配 `18083–18100`，可用调用环境 `WORKER_METRICS_ADDR` 覆盖。不要与 Docker 应用进程争用端口。

首次从旧核心异步系统迁移才执行 `ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-import`；它与本轮 AI 专用交接工具不同。宿主机 AI 迁移使用 `AI_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh ai-migrate ...`，完整分阶段命令见实现说明。

## 验证

```bash
cd backend
go build ./cmd/...
go vet ./...
go test -p=1 ./...
make proto
go generate ./api
python3 -m unittest parser_worker.test_parsers -v
cd ../web
npm run lint
CI=true npm test -- --runInBand
npm run build
```

集成测试必须使用跑过迁移的专用测试库。`TEST_POSTGRES_DSN`、`TEST_CONTROL_POSTGRES_DSN`、`TEST_AI_POSTGRES_DSN` 指向三个不同数据库；附加依赖变量与隔离边界见 AGENTS 和实现说明。未配置环境而 skip 不属于真实链路通过。不得将整个测试套件指向开发库。

`TestAISplitRealDependencies` 使用真实三库连接、MQ、MinIO、etcd、Go/Python parser、ES/QD 和 fake 模型；`deploy/tests/ai-browser.cjs` 配合该夹具验证真实浏览器交互。当前实际结果见实现说明，不代表真实模型效果、容量或跨主机 HA。

## 代码入口

- `backend/internal/aisystem`：源/文本/向量/概览、查询、发布、投影、清理、迁移。
- `backend/internal/core`、`internal/aigateway`：内部权限 RPC、公共 HTTP 适配和业务意图。
- `backend/internal/asynccontrol`、`asyncjobs`：唯一执行权威、核心提交、后继与旧消息交接。
- `backend/internal/ai/rag`：父子切片；`backend/parser_worker`：Python 解析器。
- `backend/api/proto`、`api/openapi.yaml`：契约及生成入口。
- `backend/{migrations,control-migrations,ai-migrations,archive-migrations}`：各自迁移 lane。
- `web/src`：React/Ant Design 工作台；`deploy`：配置、Compose、启动、迁移与验证工具。

历史性能快照保留在 `benchmark.md` 和 `artifacts/ai-eval/20260822T213102Z/`，对应旧架构与小数据集，不作为本次实现证据。
