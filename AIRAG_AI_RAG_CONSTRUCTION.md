# Airag AI / RAG 施工说明（Codex 执行版）

> 施工输入：`AIRAG_AI_RAG_ARCHITECTURE_DESIGN.md`
>
> 施工仓库：`/home/bronx/remote/bx_yunpan`
>
> 文档日期：2026-09-06
>
> 文档性质：实现施工手册，不是目标架构复述，也不是“已经完成”的验收报告。
>
> 初始状态：施工说明已完成，代码施工尚未因本文件自动发生。

## 0. 先分清设计稿和本次请求

`AIRAG_AI_RAG_ARCHITECTURE_DESIGN.md` 说明目标边界和升级方向。本文件把它转换为当前仓库可以逐包执行的工作项。设计稿中出现的 ES、Qdrant、ParserGateway、DocumentIR、外部 parser、shadow 和切流，除非在本仓库有源码、配置、测试和运行证据，否则一律按“待施工”处理。

本轮施工目标是：

1. 在保留当前网盘业务事实、权限、PostgreSQL/pgvector 回退和 `ai_document_versions` lease fencing 的前提下，补齐 AI/RAG 的可恢复构建链路。
2. 让外部检索平面成为可选、可重建、可回退的派生平面；不让 ES/Qdrant 成为权限或文件状态事实源。
3. 对重复投递、乱序事件、worker 崩溃、lease 过期、外部索引部分失败、服务发现故障和组件不可用做本地可重复模拟。
4. 测试只覆盖高风险主链路：单元、接口、核心集成和一次全项目热路径；不建设没有消费方的大型测试平台。
5. 保持默认开发环境能快速启动：fake AI、PostgreSQL 读路径、外部搜索 profile 关闭；受限主机使用现有串行低内存构建。

以下标记含义固定：

- `[现状]`：源码或当前运行配置已经证明。
- `[施工]`：本文件要求 Codex 实现并验证。
- `[不做]`：本轮明确不引入，避免把设计稿的长期选项提前变成复杂度。

## 1. 当前源码基线

每个施工包开始前都要重新读当前源码和 `deploy/.env`。下面是本次核对得到的基线，不得把目标状态误写成现状。

| 范围 | 当前源码事实 | 本轮缺口 |
| --- | --- | --- |
| 运行角色 | `backend/internal/bootstrap/aisvc.go` 已支持 `combined`、`query`、`indexer`；`deploy/compose.yaml` 有 `ai-query`、`ai-indexer` profile | 不重新拆 AI 进程，重点补齐契约和故障恢复 |
| AI 构建 | `backend/internal/ai/service.go` 从 MinIO 读取对象，调用 `extract.go`、切块、Embedding、enrichment，并写 `ai_chunks`/`ai_document_versions` | 抽取接口尚未收口为窄的 `ParserGateway`；阶段指标和统一错误分类需补齐 |
| 版本/租约 | `ai_documents`、`ai_document_versions`、`ai_chunks` 已存在；`startDocumentVersion`、`touchDocumentVersion` 和 active version 切换已做 lease fencing | 任务元数据和重放输入还不完整；必须继续复用现有版本系统 |
| 当前检索 | `backend/internal/ai/search.go` 直接通过 PostgreSQL FTS/pgvector 查询，`loadFullChunks` 再回权威 SQL | 查询编排还没有 ES/Qdrant 读取适配器和按请求回退 |
| 投影骨架 | `backend/internal/search/index.go` 有 `ContentProjection`、`FileProjection`、`IndexVersion`、PostgreSQL fallback；`projector.go` 有快照和版本保护 | ES/Qdrant 客户端、真实写入和外部读路径尚未接通；当前 `Projector` 只写 PostgreSQL |
| 投影消费 | `search-indexer` 通过 `outbox.ConsumeOnce` 消费 `search.project.v1`；`ConsumeOnce` 将 marker 与 PostgreSQL effect 放在同一事务 | 外部网络写入不能放进这个事务，否则会出现“已消费但外部投影未完成”；需改为可重试的两段式投影 |
| 事件/队列 | `eventcontract`、Outbox delivery、Asynq task routing、recovery 已存在；AI 任务当前主要携带 `object_id`，重处理携带 `task_id`/`object_id` | 增加可选的 `run_id`、hash、pipeline/model 版本和 trace 元数据，同时兼容旧 payload |
| 配置 | `config.Search` 已有 `Enabled`、`WriteBackends`、`ReadBackend`、`ShadowSampleRate`、`IndexEpoch`；默认 `postgres` | 外部 endpoint、超时、批量、oversample、fallback 等配置未落到 Go/Compose/脚本全链路 |
| 部署 | `deploy/up.sh --search-indexer` 只启动 PostgreSQL 投影消费者；`.env.example` 和 `.env.no-ai.example` 已有搜索基础键 | ES/Qdrant 只能作为显式 profile；不能改变默认启动和低内存构建路径 |
| 故障工具 | `deploy/local-staging-check.sh` 已覆盖 API 副本、Cache Redis、Broker/Relay、MinIO 的可逆停启；`staging-search-check.sh` 只是 staging 前置门禁 | 增加 AI/RAG 外部索引和投影恢复场景；不能把单机演练写成生产 HA 证据 |

本次只做了无写入基线验证：

```text
go test ./... -count=1       -> 45 个包，131 项通过
go vet ./...                -> 通过
bash -n deploy/*.sh         -> 通过
docker compose ... config   -> 全 profile 渲染通过
```

这些结果只说明当前源码基线可编译、单元测试和配置语法正常，不说明 ES/Qdrant、生产 HA、容量或外部 provider 已完成。

## 2. 不可违反的不变量

每个工作包的测试必须至少对应下列一个不变量；没有不变量和失败窗口的改动不进入施工。

1. **事实归属**：用户、目录、`file_entries`、`file_objects`、分享、对象 ready 状态和权限只由 core-api/PostgreSQL 决定。AI 和外部索引不能改业务事实。
2. **对象复用**：同一个物理对象只构建一份 AI 文档和向量；文件改名、移动、分享导入只更新文件投影，不重复 OCR/切块/Embedding。
3. **版本 fencing**：同一个对象最多一个有效 building lease；lease 过期或 token 不匹配的 worker 不能写入 indexed，也不能切换 active version。
4. **旧版本可用**：新版本失败、超时、provider 不支持或外部索引落后时，旧的 active version 和 PostgreSQL 回退路径仍可读。
5. **至少一次可重复**：Outbox、Asynq、投影和外部 upsert 按至少一次投递设计；重复投递只能得到同一稳定 ID 的幂等写入。
6. **版本单调**：旧 `file_version`、`acl_version`、`document_version_id` 或 `index_epoch` 的事件不能覆盖更新投影；删除先 tombstone，清理后置。
7. **权限二次确认**：ES/Qdrant 返回的只是候选。返回 Search/Ask 或送入 LLM 前，必须按当前 owner、folder/file scope、删除状态、对象 ready 状态回 PostgreSQL 确认。
8. **引用白名单**：LLM 只能看到服务端整理的 evidence；citation 只能来自本次 evidence，不能接受模型自行声明的文件或段落。
9. **故障隔离**：ES/Qdrant/AI provider/etcd/Redis cache 故障不应阻断目录、上传、下载、分享等核心网盘能力；PostgreSQL 不可用时按现有 readiness 语义失败，不伪造成功。
10. **不泄密**：不把 `DASHSCOPE_API_KEY`、外部索引密码、预签名地址或完整 DSN 写入日志、测试产物、错误响应或提交。

## 3. 施工总顺序

按以下顺序执行。后一个包不得绕过前一个包的门禁。

```text
P0 基线/契约冻结
  -> P1 ParserGateway 与解析错误分类
  -> P2 AI 任务元数据、版本和 lease 重放
  -> P3 投影消费两段式恢复语义
  -> P4 Elasticsearch/Qdrant 真实适配器
  -> P5 ai-query 外部召回、RRF、权限确认和 fallback
  -> P6 配置、Compose profile、脚本同步
  -> P7 epoch 回填、增量追平、shadow、切流和回滚
  -> P8 本地分布式故障模拟
  -> P9 分层测试与一次全项目热路径
```

每个包都执行固定循环：

```text
读当前源码/配置
  -> 写不变量和失败窗口
  -> 设计 expand/contract 与回滚
  -> 最小实现
  -> 针对性测试
  -> 运行本包门禁
  -> 保存 artifacts/airag-construction/<UTC 时间>/
  -> 更新本文状态和证据
```

## 4. P0：基线与契约冻结

### 4.1 施工文件

- 读：`backend/internal/ai/service.go`、`extract.go`、`tasks.go`、`model.go`。
- 读：`backend/internal/search/index.go`、`projector.go`、`index_test.go`。
- 读：`backend/internal/eventcontract/payloads.go`、`registry.go`、`backend/internal/outbox/routes.go`、`consumption.go`。
- 读：`backend/internal/platform/config/config.go`、`backend/internal/bootstrap/aisvc.go`、`bootstrap/roles.go`。
- 读：`deploy/compose.yaml`、`up.sh`、`config-check.sh`、`.env.example`、`.env.no-ai.example`。
- 不覆盖用户提供的 `AIRAG_AI_RAG_ARCHITECTURE_DESIGN.md`，不修改无关架构文档。

### 4.2 必须保存的基线

```bash
cd backend
go test ./... -count=1
go vet ./...
cd ..
bash -n deploy/*.sh
docker compose --env-file deploy/.env -f deploy/compose.yaml --profile app --profile ai --profile ai-split --profile distributed --profile redis-split --profile search config >/tmp/bx-yunpan-compose-config.yml
```

把命令输出、当前迁移版本、`deploy/status.sh` 和 `deploy/config-check.sh` 输出保存到本轮 artifacts。不要复制环境文件原文，尤其不要复制密钥。

### 4.3 P0 门禁

- 默认 `SEARCH_READ_BACKEND=postgres`、`SEARCH_WRITE_BACKENDS=postgres` 不变。
- `AI_PROVIDER=fake` 可以离线完成后续测试；不要求外部 API key。
- 当前测试没有被新增依赖或环境变量污染。
- 工作树中只有施工文档或本工作包明确的改动。

## 5. P1：ParserGateway 和解析质量门

### 5.1 目标

把现有 `extract.go` 包装成 ai-indexer 内部窄接口。第一阶段不建新解析微服务、不持久化完整 DocumentIR；先让输入、输出、错误和版本可观察。

### 5.2 文件边界

新增或调整：

- `backend/internal/ai/parser.go`：定义 `ParserGateway`、`ParseRequest`、`ParseResult`、质量指标和错误分类。
- `backend/internal/ai/parser_builtin.go`：把现有 builtin 抽取逻辑适配到 gateway；`extract.go` 的资源限制继续复用。
- `backend/internal/ai/service.go`：`runIndex` 只依赖 gateway，不直接拼接 MIME 分支。
- `backend/internal/ai/extract_test.go`、新增 `parser_test.go`：覆盖 preflight、质量门和错误分类。
- `backend/internal/ai/tracing.go` 或现有日志位置：为每个 parse 记录 `object_id`、`document_version_id`、`parser_id`、`parser_version`、stage、attempt、duration、error_code、trace_id。

### 5.3 最小契约

```go
type ParseRequest struct {
    RunID                uuid.UUID
    ObjectID             uuid.UUID
    DocumentVersionID    uuid.UUID
    ContentHash          string
    MediaType            string
    ObjectAddress        ObjectReadAddress // 只读地址或内部读取句柄，不传整篇文档多次
    ParserProfileVersion string
    SizeLimitBytes       int64
    PageLimit            int
    PixelLimit            int64
    TraceID              string
}

type ParseResult struct {
    ArtifactURI   string // 第一阶段允许为空；若有值必须是不可变对象
    ArtifactHash  string
    ParserID      string
    ParserVersion string
    Language      string
    PageCount     int
    BlockCount    int
    Sections      []Section // 当前可直接给 chunker 的最小结构
    Quality       ParseQuality
    Warnings      []string
}
```

实际类型以仓库风格为准，但不得把 ES/Qdrant 类型或外部 parser 客户端类型泄漏到 `Service`。

### 5.4 质量门和错误分类

- preflight：MIME/扩展名、对象大小、PDF 页数、图片像素、容器格式、是否有文本层。
- postflight：文本密度、空页比例、乱码比例、章节/表格/图片数量、OCR 置信度和 warning。
- 普通文本、源码、简单 PDF/DOCX：builtin。
- 无文本层 PDF：只有配置了真实 OCR provider 才 fallback；否则记录 `unsupported`，不无限重试。
- provider timeout、HTTP 5xx、网络断开：retryable。
- 文件损坏、非法格式、超过资源限制、不支持能力：permanent/unsupported。
- 解析成功但内容为空或质量门失败：`unsupported` 或明确的 `ai.parse_quality_failed`，保留旧 active version。

### 5.5 P1 门禁

- 相同 `ContentHash + parser_version + profile` 的成功结果可以识别并复用；不能原地覆盖不可变 artifact。
- 解析异常不会切掉旧 active version。
- 超限输入只执行一次受控失败，不进入无限重试。
- `go test ./internal/ai/... -count=1` 通过，并有至少一条 retryable 和一条 permanent 用例。

## 6. P2：AI 任务元数据、版本和 lease

### 6.1 文件边界

- `backend/internal/eventcontract/payloads.go`：给 `AIIndexRequestedV1`、`AIReprocessRequestedV1` 增加可选字段。
- `backend/internal/eventcontract/registry.go`、contract golden tests：更新 schema/兼容测试。
- `backend/internal/outbox/routes.go`：保持旧 task type 兼容，不能让新字段改变 routing。
- `backend/internal/ai/tasks.go`：解析新旧 payload；旧 payload 只有 `object_id` 时从 PostgreSQL 补齐其余字段。
- `backend/internal/ai/service.go`、`recovery/reconciler.go`：重放时带出相同的版本和稳定 run key。
- 如确需持久化新字段，新增 expand migration；先兼容旧读写，回填和验证后才考虑收缩。

### 6.2 任务输入

任务最终应能定位到：

```text
run_id
object_id
content_hash = file_objects.sha256
document_version_id（可为空，首次构建由 worker 创建）
pipeline_version
model_version
parser_profile_version
trace_id
```

`object_id + content_hash + pipeline_version + model_version + parser_profile_version + chunk_version` 是幂等边界。对象被多个 file entry 引用时，不按 file entry 重建 AI 文档。

### 6.3 lease 规则

- 继续使用 `ai_document_versions.lease_token/lease_until`，不新增第二套分布式锁。
- 每个长阶段开始前或定期 touch lease；token 不匹配、lease 过期、版本状态不是 `building` 时立即停止后续写入。
- 只有一个事务同时满足 `id + status='building' + lease_token + lease_until > now()` 才能把版本设为 `indexed` 并设置 `active_version_id`。
- worker 在“Embedding 成功但 DB 提交前”崩溃时，重试必须能重用或新建正确 generation；旧 worker 不能覆盖新 generation。

### 6.4 P2 门禁

- 新旧 payload 都能被消费；contract golden tests 不因增加可选字段而失效。
- 同一 idempotency key 的重处理不会创建第二个有效任务。
- lease 过期接管、旧 token 完成、active version 保留都有单测和 PostgreSQL focused test。
- `ai-indexer` 停止时，core-api 的目录、上传、下载和分享接口仍可用。

## 7. P3：投影消费的两段式恢复语义

这是本轮最重要的分布式施工点。

### 7.1 不能直接这样改

不能在 `Projector.Handler` 的 `db.Transaction` 内调用 Elasticsearch/Qdrant 网络客户端，也不能先调用 `outbox.ConsumeOnce` 再做外部 upsert。现有 `ConsumeOnce` 的语义是 marker 与 PostgreSQL effect 同事务提交；外部网络不可回滚，直接混入会产生以下故障窗口：

```text
外部 upsert 成功 -> 进程退出 -> PostgreSQL marker 未提交 -> 可安全重复 upsert
PostgreSQL marker 已提交 -> 外部 upsert 失败/进程退出 -> 事件不会再重试，投影永久丢失
```

第二种窗口不允许出现。

### 7.2 推荐执行算法

把一条事件的投影拆成“可重放的 canonical projection + 外部发布 + 完成 marker”：

1. 解析事件并从 PostgreSQL 读取当前事实，生成 `ContentProjection`/`FileProjection`；旧事件不能只相信 payload 中的名称和 ACL。
2. 在短事务内写入或更新对应的 PostgreSQL projection checkpoint，状态为 `pending`/`publishing`，使用 `index_epoch`、`document_version_id`、`file_version`、`acl_version` 做 CAS。此步骤不访问外部网络。
3. 事务提交后，对配置的每个外部 backend 执行稳定 ID 的 upsert/tombstone。单次请求有 deadline；部分成功可以安全重试。
4. 所有要求的 backend 成功后，再开短事务把 checkpoint 设为 `indexed` 或 `tombstoned`，然后调用 `ConsumeOnce` 记录完成。`ConsumeOnce` 冲突时只 ACK，不能重复改变较新的版本。
5. 外部调用失败、超时或进程在第 3/4 步之间退出时，handler 返回 retryable error；下一次投递根据 checkpoint 继续，并重复执行稳定 ID upsert。
6. 永久错误只标为 `failed`/`unsupported`，保留 `last_error`、attempt 和可人工重放入口；不把坏文件放入无限重试队列。
7. 如果需要避免改动 `event_consumptions` 表，允许让 projection checkpoint 作为 inbox/dedup 状态，`ConsumeOnce` 只在所有副作用成功后写入；必须新增并测试并发重复消费，不得用内存 map 去重。
8. 如果进程在 checkpoint 已是 `indexed`/`tombstoned`、但完成 marker 尚未提交的窗口退出，下一次投递或 recovery 必须先核对 checkpoint，再补写 marker；不能因为外部写已经完成就把事件丢掉。

### 7.3 投影数据和版本比较

- Content 投影键：`backend + target_version + chunk_id`；内容身份包含 `object_id`、`document_version_id`、`pipeline_version`、`model_version`。
- File 投影键：`backend + target_version + file_entry_id`；更新比较 `file_version`、`acl_version`、`index_epoch`。
- 外部索引稳定文档 ID 与 PostgreSQL projection ID 一致或可确定映射，禁止随机 ID。
- 删除先写 tombstone；只有对象没有有效 FileEntry 引用并完成 GC 后，才清理 content point/document。
- 旧 epoch 不得覆盖新 epoch；旧事件不得清除新版本的 file projection。

### 7.4 需要同步的代码

- `backend/internal/search/index.go`：把 checkpoint 的状态/attempt/error 更新能力收口。
- `backend/internal/search/projector.go`：从“事务内直接 upsert”改为上述两段式；保留 `Snapshot`/`SnapshotContent` 的 resumable checkpoint。
- `backend/internal/outbox/consumption.go`：如扩展 marker 状态，保持旧调用兼容并补充迁移；不能改变现有 core 领域消费者的原子语义。
- `backend/internal/bootstrap/roles.go`：通过依赖注入组装 PostgreSQL fallback 和外部 adapters，不在 projector 内部读取环境变量。
- `backend/internal/search/index_test.go`、新增 projector failure tests：覆盖崩溃点、重复投递、乱序和 stale CAS。

### 7.5 P3 门禁

- 在“外部 upsert 成功但 checkpoint 未提交”点杀进程，重放后最终 `indexed`，不产生重复文档。
- 在“checkpoint pending 但外部 upsert 失败”点重试，事件仍可见且能收敛。
- 同一事件并发两次消费，只有一个完成 marker，最终数据一致。
- 旧 epoch/旧 file version/旧 ACL version 事件不能覆盖新投影。
- 不允许任何外部客户端调用出现在持有 PostgreSQL 事务的代码块内。

## 8. P4：Elasticsearch/Qdrant 真实适配器

### 8.1 适配器边界

保留 `backend/internal/search/index.go` 的写入边界，在其旁边新增读取边界。建议拆成：

```go
type RetrievalCandidate struct {
    ChunkID           uuid.UUID
    ObjectID          uuid.UUID
    DocumentVersionID uuid.UUID
    FileEntryID       *uuid.UUID
    Score             float64
    IndexEpoch        int64
    PipelineVersion   string
    ModelVersion      string
    MatchType         string
}

type RetrievalBackend interface {
    SearchText(context.Context, RetrievalRequest) ([]RetrievalCandidate, error)
    SearchVector(context.Context, RetrievalRequest, []float32) ([]RetrievalCandidate, error)
    Health(context.Context) error
}
```

ES DSL、Qdrant point、HTTP response 类型只能存在 adapter 文件内，不得泄漏到 `ai.Service`、gRPC proto 或 HTTP contract。官方客户端版本必须固定在 `backend/go.mod`，先用 httptest/fake server 做 contract tests，再要求真实容器。

### 8.2 Elasticsearch

- 按 `SEARCH_INDEX_EPOCH` 创建 `files_v<epoch>`、`chunks_v<epoch>`，通过 alias 或配置选择 active epoch。
- files 至少含 `file_entry_id`、`owner_id`、`folder_id`、`object_id`、`file_version`、`acl_version`、`name`、`mime_type`、`tombstone`。
- chunks 至少含 `chunk_id`、`object_id`、`document_version_id`、`content`、`section`、`page_number`、`pipeline_version`、`model_version`、`index_epoch`。
- bulk upsert 必须支持部分失败报告；只把成功项标为 indexed，失败项保留 retryable 状态。
- 全文查询使用 bounded size 和 timeout；不从 ES 结果直接返回权限结论。

### 8.3 Qdrant

- collection 名称包含 epoch，例如 `chunks_v<epoch>`；point id 稳定为 `chunk_id`。
- vector dimension 必须与当前 `AI_EMBEDDING_DIMENSION=1024` 一致；修改模型维度必须新建 epoch，不能原地改 collection。
- payload 携带 `object_id`、`document_version_id`、`pipeline_version`、`model_version`、`index_epoch`，可带 owner/folder 作为召回加速信息，但不作为最终授权。
- 查询使用可配置 oversample；权限 SQL 过滤后不足时再补召回，不能让未授权候选占满 Top-K。
- point upsert/delete 重复执行无副作用；collection 不可用时不影响 PostgreSQL projection 和核心网盘。

### 8.4 P4 门禁

- fake/httptest adapter 能证明 mapping、bulk 部分失败、timeout、429/5xx、重复 upsert 和 stale epoch。
- 单节点 ES/Qdrant profile 可以健康检查、写入、查询和清理；服务停止时 adapter 返回可分类错误。
- `SEARCH_READ_BACKEND=postgres` 时不要求外部服务存在；`es_qdrant` 时配置校验拒绝空 endpoint。
- 真实客户端只被 search package 调用，不被业务 domain 直接依赖。

## 9. P5：ai-query 外部召回、融合和回退

### 9.1 查询链路

调整 `backend/internal/ai/search.go`，保持现有公开 Search/Ask contract 不变：

```text
参数归一化和 scope 校验
  -> ES 全文候选             \
  -> Qdrant 向量候选           -> 去重 -> RRF/weighted RRF
  -> PostgreSQL fallback      /
  -> bounded reranker
  -> parent/neighbor（只有已有数据支持时才做）
  -> evidence 数量/文件多样性限制
  -> PostgreSQL 权威权限和 object ready 确认
  -> LLM
  -> citation allow-list
```

### 9.2 fallback 规则

- `postgres`：完全保留现有 FTS/pgvector 路径，作为默认和迁移期基线。
- `shadow`：用户响应仍使用 PostgreSQL；外部查询只记录候选集合、权限差异和延迟，不改变响应。
- `es_qdrant`：外部召回成功则进入融合；任一外部 backend timeout/5xx/版本落后时，按请求回退 PostgreSQL。不能因为 ES 失败而返回空结果，也不能把未经 SQL 复核的候选送给 LLM。
- 外部和 PostgreSQL 都失败才返回 `ErrUnavailable`；错误响应不包含索引地址、密码或内部 DSL。
- 每路设置独立 timeout、最大候选数、breaker/限流；不要无限等待慢的向量服务。

### 9.3 与现有逻辑的兼容

- 继续复用 `reciprocalRankFusion`、`loadFullChunks`、reranker、每文件最多 2 个 evidence 和 citation 白名单。
- `loadFullChunks` 必须以 candidate 的 `chunk_id`、`file_entry_id` 和当前 owner 再查 PostgreSQL；不能相信外部 payload 的 owner。
- 文件改名/移动/分享导入只影响 File Projection 和 SQL 映射；Content Projection 不重新 Embedding。
- 删除、撤销分享和 ACL epoch 变化在外部索引延迟期间仍不能泄漏，因为最终权限确认回 PostgreSQL。

### 9.4 P5 门禁

- 同一固定查询集下，PostgreSQL 和外部路径得到的权限结果一致，权限泄漏为 0。
- ES down、Qdrant down、两者同时 down 都能按规则回退；目录、上传、下载、分享不受影响。
- 外部返回 stale `document_version_id` 或 `index_epoch` 时被丢弃或回退，不进入 evidence。
- Ask 的 citation 只来自当前 evidence，外部候选不能伪造文件名、页码或引用 ID。

## 10. P6：配置、Compose 和启动脚本同步

### 10.1 新配置建议

命名必须保持 `SEARCH_` 前缀，并在 Go、模板、Compose、校验和启动脚本中同时出现：

```dotenv
SEARCH_INDEX_ENABLED=false
SEARCH_WRITE_BACKENDS=postgres
SEARCH_READ_BACKEND=postgres
SEARCH_SHADOW_SAMPLE_RATE=0
SEARCH_INDEX_EPOCH=v1
SEARCH_ES_ENDPOINT=
SEARCH_ES_USERNAME=
SEARCH_ES_PASSWORD=
SEARCH_QDRANT_ENDPOINT=
SEARCH_QDRANT_API_KEY=
SEARCH_EXTERNAL_TIMEOUT=2s
SEARCH_EXTERNAL_WRITE_TIMEOUT=10s
SEARCH_INDEX_BATCH_SIZE=100
SEARCH_VECTOR_OVERSAMPLE=4
SEARCH_EXTERNAL_FALLBACK=true
```

实际字段可按现有命名调整，但必须满足：

- 默认值不启动外部服务、不改变 PostgreSQL 主读。
- `SEARCH_READ_BACKEND=es_qdrant` 要求写 backend、endpoint、epoch 和 fallback 配置完整。
- `SEARCH_EXTERNAL_FALLBACK=false` 只能在明确的 staging 对比中使用，生产默认 true。
- 密码/API key 只从环境注入，`config-check.sh` 只检查是否存在和格式，不打印值。
- timeout、批量、oversample、候选上限必须有正数和上限校验，避免错误配置拖垮单机。

### 10.2 必须同步的文件

| 变化 | 必须同步 |
| --- | --- |
| Go 配置字段 | `backend/internal/platform/config/config.go`、`config_test.go` |
| 环境模板 | `deploy/.env.example`、`deploy/.env.no-ai.example` |
| 容器环境 | `deploy/compose.yaml` 的 `x-app-environment` |
| 配置门禁 | `deploy/config-check.sh`、`deploy/up.sh`、`deploy/status.sh` |
| 外部 profile | `deploy/compose.yaml`、`up.sh` usage/参数、`down.sh`/`logs.sh`/`local-staging-check.sh` 的 profile 集合 |
| 镜像构建 | `backend/Dockerfile`、`deploy/build.sh`，只在增加新二进制时修改 |
| API/proto | 只有公开 contract 变化才同步 `api/proto`、生成物、grpc client/server、OpenAPI；内部 adapter 不改 API |

### 10.3 Compose 施工边界

- 新增 ES、Qdrant 时放入显式 `search-external` profile，单节点只用于功能、回填、shadow 和故障回退。
- 为 ES/Qdrant 设置低资源限制、健康检查、独立 volume；不得默认与 `app`/`ai` 一起启动。
- `search-indexer` 在 PostgreSQL-only 模式下仍可运行；外部 endpoint 不可用时不能让 core-api 启动失败。
- `up.sh` 增加显式 `--search-backends`（名称可保持等价，但必须在 usage、参数解析、profile、build 和 status/logs 中同步）；不把外部搜索作为默认 profile。
- `--no-build` 只能用于镜像已经包含当前代码、仅切换配置的场景；源码变更仍使用现有 `deploy/build.sh` 串行构建。
- 不在低内存主机上并行冷构建，不为了搜索 profile 修改 `GO_BUILD_PARALLELISM`、`GO_BUILD_MAX_PROCS` 的受控默认值。

### 10.4 P6 门禁

- `.env.example` 和 `.env.no-ai.example` 键集合保持对齐；无 AI 模式不要求外部搜索，但仍能迁移和启动核心网盘。
- `bash -n deploy/*.sh`、Compose 全 profile `config` 和 `config-check.sh` 通过。
- 默认启动不拉取或构建 ES/Qdrant；显式 profile 才会创建它们。
- 切换 `postgres -> shadow -> es_qdrant -> postgres` 不需要改数据库事实，不删除 volume。

## 11. P7：epoch 回填、增量追平、shadow 和切流

### 11.1 发布顺序

```text
1. 启动外部 search profile，健康检查通过
2. 生成新 index_epoch，建立 ES index/alias 和 Qdrant collection
3. 先打开增量投影，接受实时事件
4. 从 PostgreSQL active document version 和当前 FileEntry 事实做 snapshot
5. 记录 file/content 两个 checkpoint 和 snapshot watermark
6. 追平 watermark 之后的增量事件
7. 校验内容数、FileEntry 数、tombstone、版本和 checksum
8. 开启 shadow query，比较召回、权限、延迟和 fallback
9. 小比例切 SEARCH_READ_BACKEND=es_qdrant
10. 观察窗口通过后扩大；任何异常切回 postgres 或旧 epoch
```

### 11.2 回填实现

- 优先复用 `Projector.Snapshot` 和 `SnapshotContent`，不要再造第二个全量扫描器。
- snapshot 按 UUID/稳定 checkpoint 分页；中断后从 checkpoint 继续，不从头覆盖新事件。
- content 和 file 投影分开回填，因为内容按 object/document version 复用，文件投影按 file entry/ACL 变化。
- 每批写入有 bounded size 和重试；批次内部分失败必须可重放。
- active version、`document_version_id`、`pipeline_version`、`model_version`、`acl_version`、`index_epoch` 必须可审计。
- 不做停机替换，不删除旧 epoch，不回写业务表。

### 11.3 shadow 比较

固定记录：

- 候选集合差异、Recall@K/MRR/NDCG（如果有固定标注集）。
- 权限结果差异和 citation 映射差异；权限泄漏必须为 0。
- p50/p95 延迟、backend timeout、fallback 次数和 index watermark lag。
- 不记录 query 中可能含有的敏感原文；测试 query 使用固定小数据集。

### 11.4 回滚

- 外部 search 读异常：只切 `SEARCH_READ_BACKEND=postgres` 或旧 alias。
- 外部写异常：停止新的 external writer，保留 PostgreSQL projection 和未完成 checkpoint，恢复后重放。
- 不反向把 ES/Qdrant 结果写入 `file_entries`、权限表或对象状态。
- 回滚后保留失败 epoch 和 checksum，便于调查；不删除 volume 或索引，除非另有明确授权。

## 12. P8：本地分布式故障模拟

本地模拟只证明状态机和恢复语义，不证明跨主机 HA、容量或生产 RPO/RTO。所有脚本必须可逆，有 `trap` 自动恢复服务，不删除 volume/数据库/对象。

### 12.1 运行准备

使用 fake provider 和显式 profile，示例命令按施工完成后的服务名调整：

```bash
./deploy/up.sh --distributed-workers --ai-split --redis-split --search-indexer --search-backends
./deploy/status.sh
./deploy/config-check.sh
```

如果本轮不需要真实 ES/Qdrant，先以 PostgreSQL-only 运行；适配器故障使用 httptest/fake server。不要为本地演练填入或打印 `DASHSCOPE_API_KEY`。

### 12.2 必做故障矩阵

| 故障窗口 | 本地注入 | 必须观察的结果 |
| --- | --- | --- |
| Outbox 已提交、Broker 暂停 | `docker compose stop redis-broker outbox-relay`，提交一个对象/文件事件后恢复 | PostgreSQL 事件保持 pending/failed；恢复后只投递一个稳定 task，最终投影收敛 |
| 外部 upsert 成功、checkpoint 未提交 | fake adapter 在成功返回后触发进程退出或测试注入 | 重放再次 upsert 不重复；checkpoint 最终 indexed；event marker 最终只有一条 |
| checkpoint pending、外部 timeout/5xx/429 | fake adapter 按调用次数返回错误再成功 | handler 返回 retryable；attempt/last_error 可见；恢复后成功，不被 ConsumeOnce 永久吞掉 |
| 同一事件并发两次 | 两个 search-indexer handler 同时消费同一 payload | 稳定 ID/CAS 吸收重复，旧版本不覆盖新版本 |
| 乱序 file/ACL 事件 | 先投递新 `file_version`/`acl_version`，再投递旧事件 | 外部和 PG checkpoint 仍保留新版本；删除/tombstone 不回退 |
| indexer 崩溃/重启 | `docker compose stop search-indexer` 后产生事件，再启动 | 队列积压可见，恢复后 drain；active AI version 不变，核心文件 API 正常 |
| lease 过期接管 | 测试中缩短 lease，worker A 停顿，worker B 接管后让 A 恢复 | A 的旧 token 不能 indexed/active；B 的 generation 保留 |
| ES 不可用 | `docker compose stop elasticsearch` | Search/Ask 按请求回退 PostgreSQL；目录、上传、下载、分享不受影响 |
| Qdrant 不可用 | `docker compose stop qdrant` | semantic/hybrid 使用 fallback 或可用检索路；不返回越权候选 |
| ES/Qdrant 同时不可用 | 同时停止两个服务 | PostgreSQL/pgvector 主路径仍可用；仅在 PG 也失败时返回 `ai.unavailable` |
| ACL/删除事件延迟 | 先让外部候选存在，再删除/撤销分享并暂停 indexer | SQL 权限复核阻止结果返回；恢复后 tombstone 收敛 |
| etcd 注册中心不可用 | 停止 etcd，观察已连接 query 实例 | 按现有 resolver 语义继续使用最后地址或明确失败；核心网盘不受影响 |
| Redis cache/coordination 暂停 | 复用 `local-staging-check.sh` 的停启 | cache degraded 不阻断核心；leader/重试恢复后无重复事实写入 |

### 12.3 故障证据

每个场景至少保存：注入时间、恢复时间、相关服务日志（脱敏）、HTTP/gRPC 状态、PostgreSQL projection 状态、queue depth、attempt/last_error、最终 checksum。建议目录：

```text
artifacts/airag-construction/<UTC>/faults/<case-name>/
```

`LOCAL_STAGING_REHEARSAL=PASS` 只能作为本地核心故障证据；不得写成生产 HA 完成。

## 13. P9：测试分层和最终热路径

### 13.1 单元测试

只补有明确状态/边界的逻辑：

- Parser preflight、质量门和错误分类。
- `ContentHash + pipeline/model/parser profile` 幂等键。
- lease token、generation、active version、stale completion。
- ES/Qdrant mapping、稳定 ID、版本比较、tombstone、bulk 部分失败。
- Retrieval candidate 去重、RRF、oversample、fallback 和 citation allow-list。
- config 默认值、非法 endpoint/timeout/oversample 和 profile 组合。

命令：

```bash
cd backend
go test ./internal/ai/... ./internal/search/... ./internal/outbox/... ./internal/eventcontract/... ./internal/platform/config/... -count=1
```

不为简单 getter、GORM 映射或第三方客户端内部行为写测试。

### 13.2 接口测试

- gRPC `Search`/`Ask`/`Reprocess` 的输入校验、deadline、错误映射和重试策略。
- HTTP `/search`、`/ask`、`/files/:id/ai/reprocess` 的 owner scope、503 fallback 和 citation 响应。
- ParserGateway 和 RetrievalBackend 使用 httptest/fake provider，验证超时、429、5xx、部分失败和旧版本响应。
- 如果 proto/OpenAPI 没有变化，不为了内部 adapter 增加新的公开 endpoint。

命令：

```bash
cd backend
go test ./internal/ai/grpcclient/... ./internal/ai/grpcserver/... ./internal/ai -run 'Test.*(Search|Ask|Reprocess|Fallback|Citation)' -count=1
```

### 13.3 核心集成测试

使用真实 PostgreSQL/pgvector、Redis 和 MinIO；不要用 SQLite 替代 PostgreSQL。优先复用仓库的 `TEST_POSTGRES_DSN` focused test 方式，测试数据库必须独立且结束清理。

必须覆盖：

1. 空库迁移到当前版本，projection/index version 表可用。
2. 上传小文本对象 -> `object.ready` -> AI version indexed -> Search/Ask。
3. 同对象多个 file entry 只产生一份 content/chunk/vector，文件投影按 entry 分开。
4. 用户 A/B 权限隔离、分享导入、撤销分享、删除和 tombstone。
5. Outbox/Asynq 重复投递、search-indexer 重启、checkpoint 继续和 stale version。
6. ES/Qdrant 停止时 PostgreSQL fallback；外部恢复后按 epoch 追平。 

建议命令：

```bash
export TEST_POSTGRES_DSN='仅在当前 shell 注入，勿写入日志'
cd backend
go test ./... -count=1
```

如果没有真实依赖，必须明确报告“focused integration 未执行”，不能用 mock 结果代替。

### 13.4 前端和接口回归

只有 API response、AI 页面或搜索交互被改动时才运行前端门禁：

```bash
cd web
npm test -- --watchAll=false
./node_modules/.bin/eslint src --ext .js --ignore-pattern build --max-warnings=0
npm run build
```

不把 `node_modules`、`build` 压缩产物或外部 provider 输出纳入提交。

### 13.5 一次全项目热路径测试

施工结束只跑一次完整热路径，避免把本地机器变成无意义的压力测试场。使用 fake AI、小文件、小数据集、显式 distributed/AI split profile：

```text
启动并检查 status/config
  -> 注册用户 A、登录
  -> 创建目录
  -> 上传一个 UTF-8 文本（可再上传一个 PDF/图片样本）
  -> 等待 object verify 和 AI index 完成
  -> name/fulltext/semantic/hybrid Search
  -> Ask，检查 evidence 和 citation 只来自 A
  -> 创建分享，用户 B 导入并搜索
  -> 撤销分享，确认 B 的 Search/Ask 不再返回该文件
  -> A 改名/移动后检查 File Projection，不重新生成 content vector
  -> 删除 file entry，再确认仍有引用时 object 不被 GC
  -> 停止一个 ai-query/search-indexer 或外部 search backend，复测核心 API 和 fallback
  -> 恢复服务，等待队列、projection、outbox pending/failed 收敛到预期
  -> 下载原文件并做字节/sha256 校验
```

热路径必须保存：HTTP 状态、关键响应字段（去除 token/URL/密钥）、AI version、projection 状态、queue pending、对象引用计数和最终 hash。它是主链路 smoke，不是容量报告。

## 14. 同步清单

任何一个工作包改动下面任一层，都要检查对应联动，不能只改一个文件：

| 变更类型 | 同步范围 |
| --- | --- |
| event payload/版本 | `eventcontract` structs、registry、golden/contract tests、Outbox routes、AI handler、recovery |
| AI pipeline/阶段 | `ai/service.go`、`tasks.go`、`extract.go`/ParserGateway、model、metrics/tracing、AI tests |
| projection 写入 | `search/index.go`、`projector.go`、migrations、search-indexer bootstrap、checkpoint/failure tests |
| retrieval 读路径 | `ai/search.go`、candidate conversion、gRPC/HTTP error mapping、权限/citation tests |
| 配置键 | `config.go`/test、两个 env template、Compose anchor、`config-check.sh`、`up.sh`、`status.sh`、logs/profile 脚本 |
| Compose 服务 | `compose.yaml`、build target、依赖/healthcheck、status/logs/fault script、资源限制 |
| proto/OpenAPI | `.proto`、生成的 `pb`/contract、client/server conversion、前端 service、接口测试 |
| migration | up/down、空库迁移、expand/contract 兼容读写、回填/checksum、focused PostgreSQL test |
| 运行证据 | `artifacts/airag-construction/<UTC>/`，同时更新本文对应包的状态 |

## 15. 明确不做

- 不把每个文件格式拆成一个业务微服务。
- 不在当前资源条件下默认启动 Tika、DeepDoc、Docling、ES、Qdrant 全家桶。
- 不在没有重复解析成本、页码/坐标引用或多 parser 评测需求前持久化完整 DocumentIR。
- 不新建一套和 `ai_document_versions` 冲突的状态/锁表。
- 不把外部索引、Redis、Asynq、Kafka 当成权限或业务事实源。
- 不用 Kafka 替换当前短任务队列；Kafka 只有在长期 replayable domain event 有真实消费方时才启用。
- 不在外部 upsert 失败时删除 PostgreSQL active version。
- 不把 fake provider、单机 Compose、静态源码扫描或单次小数据结果写成生产性能/HA 结论。
- 不为本轮任务做全量容量压测、全浏览器矩阵、全故障注入平台或无明确消费方的治理数据库。

## 16. 最终验收清单

### 代码和边界

- [ ] `ai-query` 不消费离线 AI 队列，`ai-indexer` 不提供在线 Ask。
- [ ] Parser 通过窄接口接入；外部 parser 不直连业务写库。
- [ ] Retrieval adapter 不泄漏 ES/Qdrant 客户端类型。
- [ ] 外部网络调用不在 PostgreSQL transaction 内。

### 一致性和故障

- [ ] 重复、乱序、崩溃、超时、部分成功和 lease 接管都可重试并最终收敛。
- [ ] 旧版本/旧 epoch/旧 ACL 不能覆盖新 projection。
- [ ] 新版本失败时旧 active version 仍可服务。
- [ ] projection checkpoint、attempt、last_error、watermark 和 checksum 可查询。

### 权限和安全

- [ ] ES/Qdrant 候选经过 PostgreSQL owner/scope/object 状态复核。
- [ ] Share revoke/delete 延迟期间权限泄漏为 0。
- [ ] citation 只能来自 evidence allow-list。
- [ ] 日志、artifact、响应不含密钥、完整 DSN 或预签名 URL。

### 配置和发布

- [ ] 默认开发启动不依赖 ES/Qdrant；`postgres` 回退路径可独立运行。
- [ ] Go 配置、env 模板、Compose、config-check、up/status、profile 和测试已同步。
- [ ] `SEARCH_READ_BACKEND` 可在 `postgres/shadow/es_qdrant/postgres` 间可逆切换。
- [ ] 串行低内存 build、迁移、status、config-check 和全项目热路径各有证据。

### 测试

- [ ] 相关单元测试通过。
- [ ] gRPC/HTTP/adapter 接口测试通过。
- [ ] PostgreSQL/Redis/MinIO 核心集成测试通过，或明确记录未执行原因。
- [ ] 本地分布式故障矩阵通过并保存脱敏证据。
- [ ] 全项目热路径只执行一次并通过；没有把 smoke 当成容量门禁。

## 17. 给 Codex 的每次施工输出格式

每个工作包完成后，在回复和本文件状态记录中只写以下内容：

```text
工作包：P<n> <名称>
改动：文件路径 + 一句话职责
不变量：本次保护了哪些不变量
故障窗口：注入点、预期、实际结果
测试：实际执行的命令和通过/失败数量
证据：artifacts/airag-construction/<UTC>/...
剩余：未完成项、回滚方式、下一包前置条件
```

没有实际命令、日志、数据库状态或测试结果的内容只能标为“计划”，不能标为“完成”。
