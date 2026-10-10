# Airag AI / RAG 架构设计与升级方案

> 文档性质：面向实现和审计的架构设计稿  
> 适用范围：Airag 的 AI、RAG、文档解析、异步索引、检索和问答  
> 目标：不复制完整 RAGFlow，在保持当前业务边界的前提下，增强 RAG 的工程性、可恢复性和可演进性

## 1. 先给结论

Airag 不需要无目的堆叠模型或基础设施；本次明确增加 ES/Qdrant，是为了把 AI 检索从业务数据库中分离出来。更重要的升级，是把“文件进入系统后如何变成可检索知识”定义成一条稳定、可观察、可恢复的构建链路。

最终目标是：

~~~text
core-api
  负责用户、目录、文件、对象、分享和实际权限
        |
        | 本地事务 + Outbox
        v
Redis / Asynq
        |
        v
ai-indexer
  负责离线文档构建
        |
        +-- Preflight
        +-- ParserGateway
        +-- Chunker
        +-- Embedding
        +-- Enrichment
        +-- Index Publisher
        |
        +-- builtin extractor（当前）
        +-- Tika（需要时接入）
        +-- DeepDoc / OCR（需要时接入）
        |
        v
ai-query
  负责在线 Search / Ask
        |
        +-- Elasticsearch（全文召回）
        +-- Qdrant（向量召回）
        +-- PostgreSQL/pgvector（事实回退）
        +-- RRF
        +-- Reranker
        +-- Evidence
        +-- LLM
        +-- Citation 校验
~~~

这套设计只有四条必须稳定的边界：

1. core-api 和 PostgreSQL 掌握业务事实和权限。
2. ai-indexer 负责异步、可重试的文档构建。
3. ai-query 负责在线检索和问答，不做重型文档解析。
4. Elasticsearch 和 Qdrant 是目标检索平面，缓存和搜索投影都是派生数据，可以重建，不能反过来成为事实源。

物理上可以先使用单机 Compose，ES 和 Qdrant 各运行一个受限实例；逻辑上仍按照可拆分、可扩容的运行平面设计。未来增加机器时，主要改变 worker 和检索集群的部署位置，不改变业务协议和数据归属。

## 2. 这份设计解决什么问题

当前 Airag 已经有真实的 RAG 能力，但仍有几个边界需要继续明确：

- 文档抽取目前按 MIME 分支集中在 AI 服务内部。
- 抽取、切块、Embedding、摘要和索引写入主要由一次对象处理串联。
- 解析器输出缺少统一的长期契约。
- 外部 parser 还没有独立接入边界。
- 当前 PostgreSQL/pgvector 是已运行的检索路径；ES/Qdrant 目标检索平面尚未在本机真正接通。
- 版本和 lease 已经存在，但还没有覆盖完整的文档构建阶段。

升级目标不是把每个步骤都拆成一个服务，而是让每个步骤具备以下属性：

~~~text
输入明确
输出明确
状态可观察
失败可分类
任务可重试
结果可幂等
版本可追踪
必要时可回放
~~~

## 3. 当前实现基线

本节只描述当前源码能够证明的能力。后文的“目标”不应被误读为已经完成。

### 3.1 运行角色已经拆分

当前 AI 服务有三种运行方式：

~~~text
combined
  gRPC 查询和 ai Asynq worker 同进程，作为兼容和回滚路径

query
  只提供在线 gRPC Search / Ask，不消费 ai 队列

indexer
  只消费 ai 队列，不提供在线查询入口
~~~

相关实现位于：

~~~text
backend/internal/bootstrap/aisvc.go
deploy/compose.yaml
backend/internal/ai/backend.go
~~~

这说明在线查询和离线索引已经拥有独立的进程边界。当前不需要为了“架构完整”重新拆一遍。

### 3.2 当前对象索引流程

当前实际流程是：

~~~text
object.ready 或 ai.reprocess
  -> Outbox
  -> Asynq 的 ai 队列
  -> ProcessObject
  -> 从 MinIO 读取对象
  -> MIME 判断和文本/图片/PDF/DOCX 抽取
  -> MakeChunks
  -> 批量 Embedding
  -> summary / tags / language enrichment
  -> 事务写入 ai_chunks 和 ai_document_versions
  -> 切换 active_version_id
~~~

主要实现位于：

~~~text
backend/internal/ai/service.go
backend/internal/ai/tasks.go
backend/internal/ai/extract.go
backend/internal/ai/rag/chunk.go
backend/internal/ai/provider.go
~~~

当前 extractor 已经有文件大小、PDF 页数、文本大小、图片像素等限制，也能识别部分源码和 GB18030 文本。这些限制应当保留并继续向外部 parser 传递。

### 3.3 当前版本和租约是重要基础

当前已经存在：

~~~text
ai_documents
ai_document_versions
ai_chunks
async_tasks
~~~

ai_document_versions 已经记录 generation、pipeline_version、model_version、lease_token、lease_until、状态和 active 指针。

当前有效语义是：

- 同一个物理对象只维护一份 AI 文档事实；
- 新处理生成新的文档版本；
- 只有持有有效 lease token 的构建者可以完成版本；
- 新版本完成后才切换 active；
- 新版本失败时，旧的 active 版本继续服务。

这部分是 Airag 已经做得很好的地方。升级时应扩大它的适用范围，而不是再造一套相互冲突的锁和版本系统。

### 3.4 当前检索和问答

当前 Search 支持：

~~~text
name
fulltext
semantic
hybrid
~~~

其中：

- fulltext 使用 PostgreSQL FTS；
- semantic 使用 pgvector；
- hybrid 并行执行全文和向量召回，再用 RRF 融合；
- Ask 会重新加载完整 chunk；
- 可选使用 reranker；
- 每个文件进入 evidence 的数量受到限制；
- citation 只能来自服务端提供的 evidence。

当前查询逻辑仍直接读取 ai_chunks、ai_documents 和 file_entries。相关实现位于：

~~~text
backend/internal/ai/search.go
backend/internal/ai/reranker.go
backend/internal/ai/provider.go
~~~

权限不能只靠检索索引解决。当前实际边界是：

- 查询带有经过认证的 owner；
- SQL 过滤 owner、目录范围、文件范围、删除状态和对象状态；
- 分享导入后形成目标用户自己的 file entry；
- Ask 在发送给模型前再次从权威 SQL 加载 evidence；
- citation 由服务端根据本次 evidence 构造。

## 4. 最终架构分层

### 4.1 业务事实层

core-api 和 PostgreSQL 负责：

~~~text
用户身份
目录
file_entries
file_objects
分享和导入关系
配额和上传状态
对象 ready 状态
最终的 owner / folder / file scope 判断
~~~

AI 不能通过自己的索引结果改变这些事实。

### 4.2 AI Build Plane

ai-indexer 负责所有离线重计算：

~~~text
对象事件
  -> 任务领取
  -> 对象版本和 lease
  -> 抽取
  -> 切块
  -> Embedding
  -> enrichment
  -> chunks 和文档版本
  -> 派生搜索投影
~~~

它不接收用户在线 Ask，也不应在上传 HTTP 请求内同步解析大文件。

### 4.3 AI Query Plane

ai-query 负责：

~~~text
请求参数和范围校验
  -> 全文/向量候选召回
  -> RRF
  -> reranker
  -> evidence 去重和数量限制
  -> 权威 SQL 再确认
  -> LLM
  -> citation 过滤
~~~

ai-query 不负责创建文档版本，不直接修改解析状态，不在查询期间触发重量级 parser。

### 4.4 派生索引层

派生索引包括：

~~~text
PostgreSQL FTS / pgvector
search_content_projections
search_file_projections
Elasticsearch
Qdrant
Redis embedding cache
~~~

它们的共同特点是：

- 可以丢失；
- 可以从业务事实和 AI 文档版本重新生成；
- 不能最终决定用户能否访问文件；
- 必须携带版本、对象和文件标识，以便处理延迟和乱序。

## 5. 文档解析升级

### 5.1 ParserGateway 的定位

解析器应该先成为 ai-indexer 内部的窄接口，而不是立刻成为一组新的业务微服务：

~~~text
ParserGateway
  Parse(ParseRequest) -> ParseResult
~~~

ParseRequest 至少需要：

~~~text
run_id
object_id
content_hash
media_type
object read address
parser profile/version
size/page/pixel limits
trace id
~~~

ParseResult 不应把整篇大文档通过 gRPC 搬运多次。更合理的返回是：

~~~text
artifact_uri
artifact_hash
parser_id
parser_version
page_count
block_count
quality metrics
warnings
~~~

第一步可以只在 ai-indexer 内部定义接口，并把现有 extract.go 包装成 builtin provider。只有外部解析器真正上线时，再增加独立 HTTP/gRPC adapter。

### 5.2 Provider 不是按格式无限拆分

建议按能力和资源边界接入：

| Provider | 适合场景 | 初始形态 |
| --- | --- | --- |
| builtin | 纯文本、源码、简单 PDF/DOCX、fallback | ai-indexer 内部模块 |
| Tika | Office、HTML、EML、通用文本 | 独立 Java 容器 |
| DeepDoc/OCR | 扫描 PDF、复杂版面和表格 | 独立 Python 容器 |
| Docling/MinerU | 公式、复杂表格和布局 | 有真实样本需求时再接入 |

不建议一开始同时启动全部 provider。当前资源有限时，builtin 加一个实际需要的外部 provider 就够了。

### 5.3 Preflight 和质量门

解析前做轻量检测：

~~~text
MIME 和扩展名
文件大小
容器格式
页数
是否有文本层
是否疑似扫描件
~~~

解析后记录质量指标：

~~~text
文本密度
空页比例
乱码比例
OCR 置信度
标题/章节数量
表格数量
图片数量
解析 warning
~~~

质量门的作用不是追求复杂评分，而是识别“解析返回成功但结果不可用”的情况。

示例路由：

~~~text
普通 PDF
  -> builtin 或 Tika

无文本层的 PDF
  -> OCR / DeepDoc

表格解析明显失败
  -> 备用结构化 parser

超过资源限制或文件损坏
  -> unsupported，不重复重试
~~~

## 6. DocumentIR：什么时候做，怎么做

DocumentIR 是未来隔离 parser 和下游 RAG 的长期契约，但不要求当前立刻完整落地。

### 6.1 当前建议

当前先做两件事：

1. 为 parser 结果定义最小结构化接口；
2. 为结果增加 source hash、parser version 和质量指标。

如果现有对象级重建已经足够，就继续把最终 chunks 写入当前版本事务，不急着引入大量中间 artifact。

### 6.2 触发条件

只有出现以下事实，才把 DocumentIR 持久化到 MinIO：

- 更换 chunk 策略导致重复解析成本明显；
- 更换 Embedding 模型时不希望重新做 OCR；
- 真实文件需要页码、章节、表格或坐标引用；
- 需要对多个 parser 做离线质量比较；
- 整对象重建已经成为主要耗时。

### 6.3 最小结构

~~~text
schema_version
source_hash
parser_id
parser_version
language
pages
blocks
  block_id
  kind
  text / markdown / table
  page_number
  bbox
  section_path
  parent_id
  confidence
  warnings
stats
~~~

DocumentIR 必须是不可变结果。source hash、parser version 或 profile version 变化时生成新结果，不原地覆盖旧结果。

## 7. 索引构建状态和版本

### 7.1 不重复造状态系统

当前 ai_document_versions 已经承担对象级构建版本、active 指针和 lease fencing。第一阶段继续以它为主：

~~~text
pending
  -> processing
  -> indexed

异常：
  failed
  unsupported
~~~

抽取、切块、Embedding 和 enrichment 可以先作为同一次构建中的可观测阶段，而不是立即拆成四张新状态表。

### 7.2 何时增加阶段状态

当确实需要从中间阶段恢复时，再增加最小 stage metadata：

~~~text
document_version_id
stage
attempt
input_hash
output_uri
status
provider
provider_version
error_code
started_at
finished_at
~~~

不要在没有消费方之前，同时引入完整 workflow、artifact、评测和治理数据库。

### 7.3 active version 切换

任何新 parser、新模型或新索引都先生成新版本：

~~~text
构建新版本
  -> 校验 chunks、hash、模型和 pipeline 版本
  -> 可选 shadow
  -> 原子设置 active_version
  -> 旧版本进入 retired
~~~

失败时：

~~~text
active_version 仍保持旧版本
~~~

这保证了升级失败不会把线上问答切成空结果。

## 8. 异步、重试和幂等

### 8.1 任务输入

当前任务只有 object_id，后续 pipeline 任务应逐步带上：

~~~text
run_id
object_id
content_hash
document_version_id
pipeline_version
model_version
trace_id
~~~

这样日志、重试和回放可以定位到具体构建。

### 8.2 幂等键

建议幂等边界为：

~~~text
object_id
+ content_hash
+ pipeline_version
+ model_version
~~~

如果引入 parser profile，则再加入：

~~~text
parser_profile_version
parser_version
chunk_version
~~~

重复任务的处理规则：

- 已有相同成功版本，直接复用；
- 只有失败版本，从失败阶段或对象版本继续；
- 旧 worker 的 lease 已过期，不能切换 active；
- 相同 object 被多次引用，不重复生成 AI 文档和向量。

### 8.3 错误分类

~~~text
网络错误、provider 超时、HTTP 5xx
  -> 退避重试

provider 不支持该能力
  -> fallback provider

文件损坏、格式非法、超过大小/页数/像素限制
  -> unsupported，不无限重试

模型配额不足
  -> 延迟重试或进入待处理状态

超过最大尝试次数
  -> failed / DLQ，允许人工重放
~~~

## 9. 检索和问答目标架构

### 9.1 直接采用 ES + Qdrant 作为目标检索平面

这次升级不再把 ES/Qdrant 只当作接口预留，而是把它们列为 Airag 的明确目标组件：

~~~text
Elasticsearch
  文件名、标题和正文的全文召回
  中文分析、BM25、字段权重和短语匹配

Qdrant
  chunk 向量的 ANN/HNSW 召回
  payload 元数据过滤、分片和副本

PostgreSQL / pgvector
  业务事实、权限确认、迁移期主路径和故障回退
~~~

当前源码仍然通过 PostgreSQL FTS 和 pgvector 查询，ES/Qdrant 尚未接通。这是“当前实现”和“本次升级目标”的边界，不影响目标架构直接采用 ES/Qdrant。

采用它们的原因不是简单追求组件数量，而是把 AI 检索从业务数据库中分离出来：

- Elasticsearch 使用倒排索引和 BM25，适合全文、中文分词、短语和字段权重；
- Qdrant 使用专用向量索引，适合高维向量、HNSW、payload 过滤和独立扩容；
- 检索 CPU、内存和磁盘压力不再直接挤占 PostgreSQL 事务资源；
- 全文和向量两个数据面可以按不同负载扩容、分片和副本；
- 索引构建、重建和查询可以形成独立的故障域。

这些是有依据的工程收益，但不是未经压测的固定倍数承诺。实际收益必须用真实文档、真实查询、权限过滤和目标并发测量。

### 9.2 RetrievalBackend 不是空接口，而是实际双适配器

ai-query 依赖统一候选模型，但目标实现必须包含真正的三个 backend：

~~~text
ElasticsearchAdapter
  SearchText / SearchMetadata

QdrantAdapter
  SearchVector

PostgresFallbackAdapter
  FTS / pgvector
~~~

统一接口可以保持窄：

~~~text
RetrievalBackend
  Search(SearchRequest) -> []Candidate
  Health() -> error
~~~

不能把 ES DSL、Qdrant point 类型泄漏到问答流程。候选至少携带：

~~~text
chunk_id
object_id
document_version_id
file_entry_id（如果投影有该映射）
score
index_epoch
pipeline_version
model_version
match_type
~~~

### 9.3 两种投影，保持对象复用

当前项目将物理对象和用户可见文件分开，外部索引也必须保持这两个身份域：

~~~text
Content Projection
  chunk_id
  object_id
  document_version_id
  text / section / page
  embedding
  pipeline_version / model_version

File Projection
  file_entry_id
  owner_id
  folder_id
  object_id
  name / mime_type
  file_version / acl_version
  tombstone
~~~

推荐索引布局：

~~~text
Elasticsearch
  files_v{index_epoch}
  chunks_v{index_epoch}

Qdrant
  chunks_v{index_epoch}
  point id = chunk_id
  payload = object/document/index metadata
~~~

同一个物理对象被多个 file entry 引用时，内容 chunk 和向量只构建一次。文件改名、移动或分享导入只更新 File Projection，不重新 OCR、切块或 Embedding。

Qdrant 的 point 默认按 chunk 保持一份，用户可见的 file entry 范围通过文件投影和 PostgreSQL 权威映射确认。为防止未授权候选占满 Top-K，向量召回必须使用可配置的 oversample 倍数，并在 SQL 权限过滤后补充候选；不能把 payload 中的 owner_id 当作最终授权。

### 9.4 目标查询编排

在线目标链路是：

~~~text
问题规范化
  -> owner/folder/file scope 解析
  -> Elasticsearch 全文召回       \
  -> Qdrant 向量召回                -> 候选去重 -> RRF/weighted RRF
  -> 文件元数据/范围过滤           /
  -> reranker
  -> parent/neighbor 上下文扩展
  -> evidence 数量和文件多样性限制
  -> PostgreSQL 权威权限和对象状态确认
  -> LLM
  -> citation allow-list
~~~

ES/Qdrant 返回的是候选和分数，不返回“用户有权访问”的最终结论。RRF、reranker、evidence 数量限制和 citation 白名单继续沿用现有 Airag 逻辑。

### 9.5 Citation 的安全边界

LLM 只能接收服务端整理好的 evidence：

~~~text
候选
  -> 当前用户可见的 evidence
  -> prompt
  -> answer
  -> 只保留 evidence 中存在的 citation id
~~~

模型不能自行声明未提供的文件、章节或引用。

## 10. ES 和 Qdrant 的直接升级方案

### 10.1 架构决策

本方案将 Elasticsearch 和 Qdrant 从“未来可能接入的接口”提升为 AI 检索的明确目标实现：

~~~text
Elasticsearch
  负责全文、文件名、标题、BM25、中文分析和字段权重

Qdrant
  负责 chunk 向量、ANN/HNSW、payload 元数据过滤和向量扩展

PostgreSQL / pgvector
  负责业务事实、权限确认、迁移期兼容和故障回退
~~~

也就是说，目标状态下 ai-query 的主召回路径是 ES + Qdrant，PostgreSQL/pgvector 不再承担全部 AI 检索压力，但会保留为可靠回退路径。这样既正面获得专业检索引擎的性能和扩展能力，又不把外部索引的最终一致性风险传递给文件业务。

当前仓库尚未完成外部客户端、索引回填和真实切流，这是施工状态，不是目标架构的犹豫。目标是直接建设 ES/Qdrant；上线顺序采用可验证的渐进切换，而不是一次性删除 PostgreSQL 路径。

### 10.2 当前代码可以复用的骨架

当前已有的搜索代码可以直接作为迁移底座：

- `backend/internal/search/index.go` 已定义 `BackendES`、`BackendQdrant`、`Index`、`ContentProjection`、`FileProjection` 和 `IndexVersion`；
- `backend/internal/search/projector.go` 已有独立 `search-indexer`、对象内容投影、FileEntry 投影、ACL epoch 和 tombstone 版本保护；
- `search_content_projections`、`search_file_projections` 可作为外部索引写入的 durable checkpoint 和失败记录；
- `SEARCH_READ_BACKEND`、`SEARCH_WRITE_BACKENDS`、`SEARCH_INDEX_EPOCH` 已经提供切换和版本配置的基础；
- 当前 PostgreSQL projection fallback 可以继续承担迁移期间的主读和故障回退。

需要补齐的真实实现是：

~~~text
ElasticsearchAdapter
  mapping、bulk upsert、alias、全文查询、健康检查

QdrantAdapter
  collection、point upsert、向量查询、payload filter、健康检查

External Retrieval Orchestrator
  ES/Qdrant 并行召回、超时、部分失败、候选合并和 fallback
~~~

接口不是最终成果；最终成果是上述 adapter 被 `search-indexer` 实际写入，并被 `ai-query` 在 shadow 和小比例流量中实际读取。

### 10.3 索引布局和数据模型

索引按 `index_epoch` 建立新版本，避免新旧 mapping、模型和 pipeline 混在一起：

~~~text
Elasticsearch
  files_v{epoch}
    file_entry_id、owner_id、folder_id、object_id
    name、mime_type、file_version、acl_version、tombstone

  chunks_v{epoch}
    chunk_id、object_id、document_version_id
    content、section、page_number
    pipeline_version、model_version、index_epoch

Qdrant
  chunks_v{epoch}
    point id = chunk_id
    vector = embedding
    payload = object/document/version/index metadata
~~~

内容投影以 `object_id`/`document_version_id`/`chunk_id` 为边界，FileEntry 投影以 `file_entry_id` 为边界。一个物理对象被多个用户或目录引用时，只构建一份内容和向量；改名、移动、分享导入只更新文件投影，不重复 Embedding。

Qdrant 的向量 point 默认一份对应一个 chunk，避免按每个引用复制向量。查询得到候选后，再将 chunk 与当前用户可见的 FileEntry 映射连接。为避免未授权数据占满 Top-K，向量查询使用可配置的 oversample，权限过滤后不足时继续补召回；payload 中的 owner/folder 只能作为加速信息，不能作为最终授权结论。

### 10.4 写入、删除和版本一致性

外部索引写入必须是事务后的异步投影：

~~~text
core-api 本地事务
  -> Outbox object/file/ACL/AI active-version 事件
  -> search-indexer
  -> PostgreSQL projection state = building
  -> Elasticsearch bulk upsert / Qdrant point upsert
  -> PostgreSQL projection state = indexed
~~~

每次 upsert 都携带：

~~~text
file_version
document_version_id
pipeline_version
model_version
acl_version
index_epoch
~~~

旧版本事件不能覆盖新版本投影。删除先写 tombstone，再异步清理；只有对象没有有效 FileEntry 引用并进入 GC 时，才删除 object 内容和向量。外部写入失败可重试，重复写入由稳定文档 ID、版本比较和 projection state 吸收。只有外部 upsert 成功后才把投影标为 indexed；如果进程在两步之间退出，下一次任务会重放幂等 upsert。

### 10.5 从 PostgreSQL 迁移到 ES/Qdrant

迁移不是停机替换，而是一次可回滚的索引发布：

~~~text
1. 启动单节点 ES 和 Qdrant，配置资源限制和健康检查
2. 创建新的 index_epoch、ES index/alias 和 Qdrant collection
3. 打开增量投影，让新事件先进入 search queue
4. 从 PostgreSQL active document version 和 FileEntry 事实做 snapshot 回填
5. 追平 snapshot watermark 之后的增量事件
6. 校验内容数、FileEntry 数、tombstone、版本和 checksum
7. 开启 shadow query，对比召回集合、权限结果和延迟
8. 小比例将 ai-query 读路径切到 es_qdrant
9. 外部索引异常立即 fallback PostgreSQL/pgvector
10. 观察窗口通过后扩大流量，保留旧 epoch 作为回滚版本
~~~

这里的“直接采用”指目标架构和施工方向直接采用 ES/Qdrant，不指绕过回填、影子查询和回滚门禁。外部索引没有 PostgreSQL 的事务语义，必须用 epoch、版本和幂等投影补上这部分工程控制。

### 10.6 性能收益和验收口径

预期收益来自检索职责专业化和资源隔离：

- ES 的倒排索引、BM25、中文 analyzer 和字段权重更适合全文搜索；
- Qdrant 的 HNSW/ANN、payload filter、分片和副本更适合向量搜索；
- 检索节点与 PostgreSQL 事务节点独立，向量索引不会直接争抢业务库内存和 CPU；
- ES、Qdrant、ai-query 和 ai-indexer 可以按不同负载分别扩展；
- bulk 写入、segment/collection 缓存和专用索引结构可以降低大规模检索压力。

不能在没有数据集和压测前写“固定提升 10 倍”。上线验收至少比较：

~~~text
相同文档集和查询集
  p50 / p95 / p99 延迟
  Recall@K、MRR/NDCG
  PostgreSQL CPU、IO 和连接数
  ES/Qdrant CPU、内存、磁盘和队列延迟
  索引新鲜度和 watermark lag
  fallback 次数
  权限泄漏数量（必须为 0）
~~~

如果 ES/Qdrant 在真实负载下没有达到延迟、吞吐或资源隔离目标，就调整 mapping、HNSW、分片、批量大小和 oversample，而不是凭架构名称宣称性能提升。

## 11. 权限和数据安全

本方案严格沿用当前业务模型，不增加额外业务角色。

### 11.1 当前权限路径

~~~text
认证请求
  -> 得到 owner
  -> Search 按 owner / folder / file scope 查询
  -> Ask 再次加载授权 chunk
  -> 只把授权 evidence 发送给模型
~~~

分享导入后的文件按照目标用户拥有的 file entry 参与检索。物理对象可以复用，但不能因为对象相同就绕过目标用户的文件范围。

### 11.2 Parser 安全边界

解析器处理的是不可信文件，因此需要：

- 文件大小、PDF 页数、图片像素和解压缩资源限制；
- 独立进程或容器；
- 受限运行用户；
- 只读根文件系统；
- 临时目录配额；
- 默认禁止无关外网；
- parser 不直连业务写库；
- MinIO 短时效读取地址；
- 错误信息不泄露本地路径和密钥。

这些是文件处理的进程安全要求，不改变现有业务权限模型。

## 12. 分布式部署和扩缩容

### 12.1 单机起步

当前机器资源有限时：

~~~text
core-api
ai-query
ai-indexer
search-indexer
Elasticsearch（单节点、search profile）
Qdrant（单节点、search profile）
PostgreSQL
Redis / Asynq
MinIO
~~~

ES/Qdrant 是目标检索平面，即使在单机环境也应通过独立容器和独立资源限制运行。Tika、DeepDoc 等解析器仍作为按需 profile，不默认全部启动。当前机器不足以同时运行多副本搜索集群时，单节点只用于功能、回填和故障回退验证，不宣称生产高可用。

### 12.2 多机形态

资源和数据规模增长后：

~~~text
节点 A
  core-api / ai-query

节点 B
  ai-indexer / search-indexer

节点 C
  Elasticsearch data/coordinating nodes

节点 D
  Qdrant nodes / OCR worker
~~~

扩容依据：

- ai-query：在线 QPS 和 P95 延迟；
- ai-indexer：ai 队列积压和模型限流；
- search-indexer：投影积压、bulk 延迟和外部写入失败率；
- Elasticsearch：全文查询延迟、segment 合并、磁盘和 JVM 内存；
- Qdrant：向量查询延迟、内存、collection 水平和 payload 过滤耗时；
- Tika：通用解析队列积压；
- DeepDoc：PDF/OCR 队列、CPU、内存或 GPU。

解析 worker 保持近似无状态：

- 不保存会话；
- 不把本地磁盘当事实；
- 输入来自 MinIO；
- 输出写入数据库或 artifact；
- 实例可以随时增加、减少或替换。

重型 parser 的模型缓存只能作为本地加速，不能作为唯一数据来源。

### 12.3 etcd 的边界

异步 parser worker 不需要依赖 etcd 做任务分发，Asynq 已经负责队列竞争。

ES 和 Qdrant 的集群成员、分片和副本由各自的集群机制或 Kubernetes StatefulSet 管理，不通过 etcd 代替它们的存储协调。etcd 仍只用于当前 aisvc gRPC 服务发现，或裸机环境中确实需要的 parser/AI 实例发现。

建议职责：

~~~text
Compose / Kubernetes
  进程、资源和固定服务地址

PostgreSQL
  解析配置、版本、任务元数据和审计

Redis / Asynq
  队列、重试和协调

etcd
  只有裸机多主机且需要动态实例发现时才使用
~~~

当前 etcd 可以继续服务 aisvc gRPC 注册发现，但不需要为 parser 另外建设配置中心。

## 13. 可观测性和运维

每次索引构建应至少能观测：

~~~text
object_id
document_version_id
pipeline_version
parser_id/version
stage
attempt
duration
error_code
trace_id
~~~

建议指标：

- ai 队列深度；
- 各阶段处理耗时；
- parser 成功率和 fallback 次数；
- unsupported 和 failed 数量；
- lease expired 数量；
- Embedding 调用次数和配额错误；
- active version 切换次数；
- Search 的 FTS、向量、RRF、rerank 耗时；
- citation 被过滤的数量；
- 外部索引 shadow 差异和 fallback 次数。

告警重点不是“某个服务返回 200”，而是：

~~~text
任务是否积压
失败是否持续增加
旧版本是否长期未更新
外部索引是否落后
权限复核是否出现异常
~~~

## 14. RAGFlow 的借鉴范围

本次用于静态分析的 RAGFlow 源码临时 clone 位于：

~~~text
/home/bronx/remote/ragflow
~~~

远程仓库：

~~~text
https://github.com/infiniflow/ragflow.git
~~~

重点借鉴：

1. PDF 不应简单地当成一串无结构文本。
2. 页面、章节、表格、图片和版面信息对召回和引用有价值。
3. 文档处理应该是可观察的任务，而不是一个黑盒函数。
4. 复杂 PDF、OCR 和表格能力适合通过独立 provider 接入。
5. 解析成功不代表结果质量足够，需要质量指标和 fallback。

不直接照搬：

- 不复制完整 RAGFlow 平台；
- 不绑定 RAGFlow 内部数据模型；
- 不让外部 parser 直接写 Airag 业务数据库；
- 不为了增加组件数量引入不必要的服务；
- 不把 RAGFlow 的运行时规模假设成 Airag 当前需求。

当前条件不足以完成完整 RAGFlow 部署，因此关于 RAGFlow 的判断以源码静态分析为主，不能据此宣称其生产性能或故障行为已在本环境验证。

## 15. 推荐实施顺序

ES/Qdrant 是本方案确定的目标，不再以“等瓶颈出现再考虑”作为是否建设的前提。需要渐进的是写入、回填和读流量切换，因为外部索引没有 PostgreSQL 的事务语义。

### 阶段一：收口现有能力并准备外部检索

目标：

- 保持 ai-query / ai-indexer split；
- 保持当前 active version、lease fencing、owner 过滤和 citation 校验；
- 实现 ElasticsearchAdapter、QdrantAdapter 和统一候选模型；
- 增加 ES/Qdrant endpoint、超时、批量大小、oversample 和 index epoch 配置；
- 在 Compose 中增加单节点 ES、Qdrant 和 search profile，设置资源限制与健康检查；
- 保留 PostgreSQL/pgvector 作为当前主读和回退。

验收：

- adapter 可以独立健康检查和幂等 upsert；
- 外部服务停止不影响上传、目录、分享和下载；
- ai-query 仍可完整走 PostgreSQL/pgvector；
- ES/Qdrant 的数据不会进入 core-api 本地事务。

### 阶段二：解析契约与索引投影并行施工

目标：

- 在 ai-indexer 内部定义 ParserGateway；
- builtin 作为默认 provider，按真实样本接入一个 Tika 或 DeepDoc provider；
- 为 parser 结果增加 source hash、parser version 和质量指标；
- search-indexer 从 active document version 和 FileEntry 事实构建 ES/Qdrant 投影；
- 内容投影和 FileEntry 投影分开，改名/移动/分享导入不重新 Embedding。

验收：

- parser 失败可回到 builtin 或旧 active version；
- 同一 object 不重复生成向量；
- ES/Qdrant 投影携带 document/file/index 版本；
- 重复事件和乱序事件不会覆盖更新版本。

### 阶段三：全量回填、增量追平和 shadow

目标：

- 创建新的 `index_epoch`、ES index/alias 和 Qdrant collection；
- 先打开增量投影，再从 PostgreSQL 做 snapshot；
- 按 watermark 追平 snapshot 之后的事件；
- 校验内容数、FileEntry 数、tombstone、active version 和 checksum；
- 对相同查询并行执行 PostgreSQL 与 ES/Qdrant，记录召回集合、权限结果和延迟差异。

验收：

- snapshot 中断可以从 checkpoint 继续；
- snapshot 不会覆盖较新的实时事件；
- ES/Qdrant 停止时 PostgreSQL/pgvector 继续可用；
- shadow 查询不改变用户响应，权限泄漏为零。

### 阶段四：切换 ES/Qdrant 主读

目标：

- 通过 `SEARCH_READ_BACKEND=es_qdrant` 和 feature flag 小比例切换；
- ES 负责全文召回，Qdrant 负责向量召回，ai-query 负责 RRF、reranker 和 evidence；
- 外部索引超时、不可用或版本落后时，按请求回退 PostgreSQL/pgvector；
- 保留旧 epoch、PostgreSQL projection 和回退配置，完成观察窗口后再扩大流量。

验收：

- 新旧路径的 Recall@K、MRR/NDCG、P95 延迟达到既定目标；
- 删除、移动、分享导入和对象 GC 的投影语义正确；
- 权限复核和 citation 结果与 PostgreSQL 基线一致；
- 回退和再次切换可以演练成功。

### 阶段五：按负载扩展检索集群

目标：

- Elasticsearch 从单节点扩展到多节点、分片和副本；
- Qdrant 按 collection、分片、副本和量化策略扩展；
- search-indexer、ai-query、ES、Qdrant 按各自指标独立扩容；
- 建立快照、恢复、重建和节点故障演练。

这一阶段是容量和高可用扩展，不是是否采用 ES/Qdrant 的决策点。是否增加节点、分片和副本，必须由真实数据规模、SLO 和故障演练决定。

### 阶段六：质量闭环

在 ES/Qdrant 主读稳定后，再加入最小离线问答集、shadow build、canary 和用户反馈。质量闭环用于比较 parser、chunk、embedding 和 retrieval 版本，不阻塞前面检索平面的建设。

## 16. 明确不做的事情

- 不把每种格式拆成一个业务微服务；
- 不把 parser 做成新的业务事实源；
- 不在当前资源条件下同时运行多个重型 parser；
- 不把 ES/Qdrant 当成权限或文件状态数据库；
- 不把所有阶段都提前变成独立数据库和工作流；
- 不用 Kafka 替换 Asynq 的短期任务语义；
- 不为了架构图引入当前业务没有消费方的治理模型；
- 不把尚未验证的外部组件写成当前已实现能力。

## 17. 审计验收清单

### 代码边界

- ai-query 是否不消费离线索引队列？
- ai-indexer 是否不承担在线 Ask？
- parser 是否通过统一窄接口接入？
- parser 是否不直连业务写库？
- 大文件是否避免在多个 RPC 层复制？

### 一致性

- 同一个 object 是否最多只有一个有效 building lease？
- 过期 lease 是否不能切换 active？
- 新版本失败时旧 active 是否保持可读？
- 重复 Outbox/Asynq 是否不会产生重复有效索引？
- 删除和分享导入是否保持 object 与 file entry 的边界？

### 权限

- Search 是否始终带 owner、目录或文件范围？
- Ask 是否重新加载授权 chunk？
- citation 是否只能来自当前 evidence？
- 外部索引返回结果是否仍需业务事实确认？
- 派生索引是否不能绕过当前 file entry 关系？

### 解析安全

- 文件大小、页数、像素和解压缩限制是否有效？
- parser 是否有进程资源限制？
- parser 是否无法写业务事实？
- 超限和损坏文件是否不会无限重试？
- 错误日志是否不会泄露凭据和本地路径？

### 扩展和回退

- 新 parser 是否有真实样本和质量对比？
- DocumentIR 是否解决了可测量的重复解析问题？
- ES/Qdrant 是否可以从事实库重建？
- shadow 和切换是否保留 PostgreSQL fallback？
- 没有明确收益的服务和基础设施是否被拒绝？

## 18. 面试表达

Airag 没有试图复制完整 RAGFlow，而是把 RAG 的工程边界做实。core-api 和 PostgreSQL 继续负责文件事实和权限，Outbox/Asynq 驱动 ai-indexer 做异步文档构建，ai-query 负责在线召回、RRF、重排和问答。文档解析通过 ParserGateway 接入，复杂 parser 可以独立进程扩缩容，但解析结果不会直接成为业务事实。目标检索平面明确采用 Elasticsearch + Qdrant：ES 负责全文召回，Qdrant 负责向量召回，search-indexer 异步投影，ai-query 负责融合、重排和证据编排。新索引先构建、回填、校验和 shadow，最后才切 active；外部索引只提供候选，最终权限仍回到业务事实确认，PostgreSQL/pgvector 永久保留为可靠回退。这样系统不追求组件最多，但具备可替换、可重试、可回放、可回退和可扩展的生产工程基础。

## 19. 最终判断

Airag 当前最合理的 AI 架构不是“全面平台化”，而是：

~~~text
core-api / PostgreSQL
  业务事实和权限

ai-indexer / Asynq
  对象级异步构建和版本 fencing

ParserGateway
  条件接入 builtin、Tika、DeepDoc 等解析能力

ai-query
  ES + Qdrant 检索、RRF、rerank、evidence、LLM

search-indexer
  ES/Qdrant 异步投影和版本发布

ES / Qdrant
  目标全文/向量读模型
~~~

先建设 ES/Qdrant 单节点目标链路和 PostgreSQL fallback，再通过回填、shadow、切流和故障演练逐步扩大规模。parser、DocumentIR 和多节点集群按实际收益推进，但 ES/Qdrant 不再只是埋接口。这样升级是可执行的，也不会让 Airag 背上当前业务并不需要的额外业务复杂度。
