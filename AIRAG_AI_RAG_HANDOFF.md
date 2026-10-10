# AIRAG / RAG 施工交接

交接时间：2026-09-08 UTC

施工依据：`AIRAG_AI_RAG_CONSTRUCTION.md`。该文件是施工指令；本文件是本轮实际执行结果，不把设计目标写成已完成事实。

## 本轮结论

P1-P9 的源码、配置、迁移和本地可重复故障语义已落地；P9 的单元、接口、PostgreSQL focused、本地 fake/httptest、真实 ES/Qdrant 单节点门禁、一次完整 HTTP 热路径和真实外部读/故障语义已通过。默认读取仍是 PostgreSQL，ES/Qdrant 仍是可选派生平面；`realv3` 已完成外部回填并通过一次临时切流验证，随后恢复默认读路径。

当前 Compose 已用最新源码相关镜像运行：2 个 API、2 个 `ai-query`、1 个 `ai-indexer`、`search-indexer`、四个独立 worker、三套 Redis、PostgreSQL、MinIO、etcd 和 Web 均通过健康/metrics 探针。迁移为 `21|false`，Outbox pending/failed 为 `0/0`，search projection checkpoint 行数为 `2`。默认运行态的搜索读取和写入均为 PostgreSQL。

## 2026-09-09 续施工结果

- 重新执行 `go test ./... -count=1`：148 项、46 个包通过；`go vet ./...`、`bash -n deploy/*.sh`、`git diff --check` 和 Compose 全 profile 渲染均通过。
- 发现并修复了运行态控制面缺口：默认 PostgreSQL 投影虽有数据，但没有 active generation。执行 `./deploy/search-backfill.sh --mode backfill --target-version v1 --index-epoch 1 --batch-size 100 --activate` 后，PostgreSQL file/content 版本均为 `active`，快照计数为 `23/60`，cursor 和 SHA-256 checksum 已持久化。
- `./deploy/status.sh` 和 `./deploy/config-check.sh` 通过；2 个 API、2 个 `ai-query`、1 个 `ai-indexer`、4 个独立 worker、`search-indexer`、Web、PostgreSQL、MinIO、etcd 和三套 Redis 均正常。当前 `search-indexer` 仍明确使用 `SEARCH_READ_BACKEND=postgres`、`SEARCH_WRITE_BACKENDS=postgres`。
- VPN 后重新拉取 Qdrant、Elasticsearch、pgvector、Redis、MinIO、etcd、Go/Alpine 基础镜像，以及 OTEL Collector、Jaeger、Prometheus、Grafana 均成功；只通过命令行临时使用 `docker.m.daocloud.io`，未修改 `deploy/.env`。
- 修复 ES 8.19 重复建索引返回 `400 resource_already_exists_exception` 的幂等识别，并补充 mapping 错误不被吞掉的 httptest；`go test ./... -count=1` 结果为 148 项/46 包。
- `realv3/index_epoch=3` 已真实回填并激活：PostgreSQL、Elasticsearch、Qdrant 六条版本均为 `active`，file/content 快照为 `23/60`，三端 checksum 一致；临时 `es_qdrant` 读配置下真实 API Search/Ask 均 HTTP 200，分别命中 1 条和返回 1 条授权 citation，随后已恢复 PostgreSQL 默认读写。
- `deploy/up.sh --no-build` 已修复为把 `--no-build` 传给 Compose；使用镜像源串行构建 `migrate:local`、`aisvc:local`、`search-indexer:local` 后，默认 `up.sh --no-build` 成功启动，迁移 `21|false`，Outbox pending/failed `0/0`，所有 status/config-check 门禁通过。
- 详细证据：`artifacts/airag-construction/20260909T031841Z/continuation.md`。

## 本轮改动

- `backend/internal/ai/parser*.go`：ParserGateway、builtin parser、错误分类和质量门。
- `backend/internal/ai/search.go`、`tracing.go`、`shadow.go`：外部候选回 PostgreSQL 权限/active-version/object-ready 复核，shadow 脱敏摘要、fallback 和 citation allow-list。
- `backend/internal/ai/tracing.go`、`search_test.go`：将外部派生索引的可重试不可用错误归一为 `ai.ErrUnavailable`，保证 fail-closed HTTP 503 业务码稳定。
- `backend/internal/search/external.go`、`retrieval.go`：ES/Qdrant 窄适配器、bounded KNN、NDJSON bulk 部分失败、Qdrant 维度校验、稳定 ID；epoch 建索引顺序已固定。
- `backend/internal/search/retrieval.go`、`external_test.go`：保留上游错误类型/原因，区分 ES 已存在索引与真正 mapping 错误。
- `backend/internal/search/projector.go`、`backfill.go`：canonical PostgreSQL projection、两段式外部发布、checkpoint、watermark/checksum、UUID cursor、activate/rollback。
- `backend/migrations/000020_*`、`000021_*`：投影 checkpoint 和回填元数据。
- `backend/internal/search/projector_failure_test.go`、`index_test.go`：外部失败恢复、崩溃窗口、并发重复消费、rollback focused 测试。
- `backend/internal/ai/service.go`：修复成功索引后兼容 `ai_documents` 摘要更新漏绑定 `document.ID` 的 PostgreSQL 参数错误。
- `deploy/airag-fault-matrix.sh`、`deploy/search-backfill.sh`：本地故障矩阵和回填控制面。
- `deploy/config-check.sh`：新增搜索键缺省值归一化，避免用户 `.env` 未显式写新键时误报配置漂移。
- `deploy/compose.yaml`：为官方最小 Qdrant 镜像改用可用的 bash TCP readiness probe；外部 profile 仍保持显式 opt-in。
- `deploy/up.sh`：`--no-build` 现在会传递给 Compose，缺失镜像时不再隐式访问 registry/build。

## 实际验证

通过：

- `go test ./... -count=1`：148 项，46 个包；使用当前 PostgreSQL focused DSN。
- `go vet ./...`：通过。
- `go test ./internal/search/...`：17 项 PostgreSQL/fake/httptest focused 通过。
- 真实 ES/Qdrant 容器 probe：通过；健康、epoch mapping/collection、稳定 ID 重复 upsert、ES text 召回和 Qdrant vector 召回均通过。测试索引/collection 已删除，volume 保留。
- 完整 HTTP 热路径：通过；注册 A/B、目录、MinIO multipart、对象校验、fake AI indexed、Search、Ask citation、分享公开访问/撤销、B 导入、改名/移动/删除、readiness 和下载 SHA-256 校验均通过。脱敏日志：`artifacts/airag-construction/20260907T-hotpath/hotpath-full.log`。
- `bash -n deploy/*.sh`、`git diff --check`：通过。
- Compose 全 profile `config`：通过。
- `./deploy/status.sh`：API readiness、AI gRPC health、AI indexer、Web/API/Storage、四个 worker metrics、search-indexer metrics 全通过。
- `./deploy/config-check.sh`：通过。
- `migrate version`：`21|false`。
- `./deploy/airag-fault-matrix.sh`：`LOCAL_AIRAG_FAULT_MATRIX=PASS`，真实 ES/Qdrant stop/restore 已执行。最新证据目录：`artifacts/airag-construction/20260907T024100Z/faults/`。
- 真实 API 外部读/故障验证：外部停止且 `SEARCH_EXTERNAL_FALLBACK=false` 返回 HTTP 503 / `ai.unavailable`；外部停止且 fallback 开启时回 PostgreSQL，待 AI 文档 indexed 后命中 1 条；外部健康且 `SEARCH_READ_BACKEND=es_qdrant`、fallback 关闭时 Search 命中 1 条、Ask 返回 1 条授权 citation。脱敏证据：`artifacts/airag-construction/20260908T014153Z/faults/external-api-fallback.log` 和本次 continuation。

故障矩阵覆盖：外部 adapter timeout/429/5xx、checkpoint 失败重试、外部发布成功后的 marker 崩溃窗口、同事件并发重复消费、PostgreSQL index version rollback。证据明确标记为本地/fake/httptest/PostgreSQL 语义，不是生产 HA 或容量结论。

## 镜像和运行边界

官方 Docker Hub 本轮仍出现 TLS timeout。使用命令行临时覆盖 `DOCKER_REGISTRY=docker.m.daocloud.io`、`GO_PROXY=https://goproxy.cn,direct`，未修改 `deploy/.env` 或仓库默认源；本轮按 `deploy/build.sh` 串行低内存方式重建了修复后的 `aisvc`，并通过 `deploy/up.sh --no-build --distributed-workers --ai-split --redis-split --search-indexer --api-replicas 2 --aisvc-replicas 2` 恢复运行态。

未打印、未保存 `DASHSCOPE_API_KEY`、密码、完整 DSN 或预签名 URL。

## 尚未执行 / 不应宣称完成

- 默认在线读路径仍为 PostgreSQL；`realv3` 外部 active 只代表通过本地 snapshot/checksum/临时 API 切流门禁，不代表外部索引成为权限或业务事实源。ES/Qdrant 容器已按 opt-in 规则停止，volume 保留。
- 没有把一次临时切流扩展成生产 HA、跨主机、高可用、容量或 RPO/RTO 结论。
- 真实 API fallback 已在单机本地完成验证，但没有把该证据扩展成跨主机、高可用或容量结论。
- 没有生产 HA、跨主机故障、容量、RPO/RTO、真实 provider、Kafka 或专用 staging 结论。

## 接手命令

```bash
cd /home/bronx/remote/bx_yunpan
./deploy/status.sh
./deploy/config-check.sh
./deploy/search-backfill.sh --mode status --target-version v1
```

外部搜索继续施工时，先在专用 staging 注入 endpoint，再执行 snapshot -> watermark drain -> checksum -> shadow -> 小比例切换；异常只切回 `SEARCH_READ_BACKEND=postgres`，不删除 volume、不回写业务事实。

## 工作树注意

当前工作树包含本轮源码、迁移、部署脚本、施工文档和 artifacts 的未提交改动。不要 `git reset`、`git checkout` 或清理数据库、Redis、MinIO、Docker volume、镜像和证据目录。
