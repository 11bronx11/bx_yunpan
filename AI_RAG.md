p         1 # AI / RAG 模块说明

本文以当前代码为准，简要说明网盘 AI 模块的架构、实现和 RAG 原理。部署步骤、环境变量完整说明和运行手册仍以 [README.md](README.md) 为准。

## 1. 总体架构

```text
浏览器
  |
  v
API (鉴权、参数校验、权限边界)
  |
  +-- HTTP Ask/Search
  |
  +-- gRPC client -> etcd 服务发现 -> aisvc
                                  |
                                  +-- PostgreSQL / pgvector
                                  +-- MinIO
                                  +-- Embedding / LLM / Reranker Provider
```

`aisvc` 是独立的 AI 服务，承载索引、搜索、问答和重排。API 不直接访问模型服务，而是通过 gRPC 调用 aisvc；客户端使用 etcd 注册的实例并做 round-robin。AI 服务不可用时，普通网盘功能仍可继续，AI 请求返回可识别的 unavailable 错误。

AI 数据按物理对象保存，而不是按引用保存：

- `ai_documents.object_id` 唯一，一个对象只有一份 AI 文档记录。
- `ai_chunks` 保存该文档的所有切片、页码、章节、全文检索字段和向量。
- `file_entries` 是用户可见的引用。同一对象被多个目录或账户引用时，共享同一份 AI 文档和 chunks；检索时再按当前用户权限过滤。

## 2. 索引链路

文件对象完成后，上传事务通过 Outbox 投递异步任务，Worker 调用 `aisvc.ProcessObject`：

```text
对象完成
  -> Outbox / Worker
  -> 获取对象元数据并从 MinIO 读取
  -> MIME 判断与文本抽取
  -> 结构化切块
  -> 批量 Embedding（默认每批 10 个）
  -> 摘要、标签、语言 enrichment
  -> 事务写入 ai_chunks 和 ai_documents
```

索引状态为 `pending`、`processing`、`indexed`、`failed` 或 `unsupported`。对象过大、类型不支持、内容非法等情况会落到 `unsupported`；模型调用或持久化错误会落到 `failed`。处理期间会续租 AI document，避免重复 Worker 同时写入；重新索引通过任务幂等键去重，并由同一对象级 lease 串行化。

关键实现：

- `backend/internal/ai/service.go`：对象索引、任务、lease 和最终事务写入。
- `backend/internal/ai/provider.go`：Embedding、enrichment 和 LLM Provider。
- `backend/internal/ai/grpcserver/server.go`、`grpcclient/client.go`：aisvc RPC 边界。

## 3. 结构化切块

切块入口是 `backend/internal/ai/rag/chunk.go` 的 `MakeChunks`。算法按以下优先级保留文档结构：

1. 规范化换行和空白，按段落形成结构单元。
2. Markdown 标题或短标题会与后续段落绑定，保留上下文。
3. 在不超限的前提下合并段落；长段落优先按中文或西文句末拆分。
4. 单句仍超限时才进行 rune 级硬切，并保留 overlap。

每个 chunk 上限为 `1200 rune`，overlap 为 `160 rune`，单文档最多 `256` 个 chunk。每个切片同时保存 `page_number` 和 `section`，因此检索结果可以回指原文位置，而不是只显示一段无来源文本。

## 4. 检索与 RRF

`Search` 支持 `name`、`fulltext`、`semantic` 和 `hybrid` 四种模式：

- `name`：文件名精确、前缀和模糊匹配。
- `fulltext`：PostgreSQL 全文检索（FTS）。
- `semantic`：查询先做 Embedding，再使用 pgvector 相似度检索。
- `hybrid`：FTS 与向量检索并行执行。

混合检索先为每一路保留最多 `50` 个候选，再用 Reciprocal Rank Fusion（RRF）按名次融合，融合池最多 `30` 个 chunk，最后才应用调用方的 `limit`。这样可以同时利用关键词命中和语义相似度，避免任一路的单一评分尺度主导结果。

内部 candidate 会保留完整 chunk 内容；HTTP/gRPC 对外只暴露文件信息、分数、匹配类型和 citation 摘录。Ask 阶段会依据授权范围再次从 SQL 加载完整内容，避免把数据库中不可见的 chunk 带入模型。

相关实现：`backend/internal/ai/search.go` 中的 `Search`、`queryCandidates`、`reciprocalRankFusion`，以及 `backend/internal/ai/tracing.go` 中的检索埋点。

## 5. Ask、Reranker 与 Citation

问答链路如下：

```text
Ask
  -> hybrid Search
  -> 去重后的 RRF candidate（默认最多 30）
  -> SQL 再次加载授权 chunk
  -> Reranker（可选）
  -> 每文件最多 2 个，最多 8 个 evidence
  -> LLM 生成 answer / grounded / citation IDs
  -> citation 白名单校验
```

Reranker 只参与 Ask，不改变普通 Search 的返回顺序。`AI_RERANK_ENABLED=true` 时，aisvc 将 RRF 候选发送到配置的重排服务；DashScope `qwen3-rerank` 使用 `AI_RERANK_BASE_URL` 的 `compatible-api/v1` 根地址，客户端自动追加 `/reranks`。重排失败会保留原候选顺序继续问答，避免 Reranker 故障扩大为整个 AI 不可用。

LLM 输出中的 citation ID 只能引用本次授权 evidence 中存在的 ID。未知 ID 和重复 ID 会被丢弃；当 `grounded=false` 时，后端清空全部 citations；非 JSON 输出会走降级解析。因此 citation 是服务端根据证据集合重新构造的，不信任模型直接提供的文件信息。

默认重排参数为：候选 `30`、送入 LLM 的 Top-K `8`、每个文件最多 `2` 个 evidence、HTTP 超时 `30s`。配置入口为 `deploy/.env*`、`deploy/compose.yaml` 和 `backend/internal/platform/config/config.go`。

## 6. 权限、复用与一致性

检索 SQL 以 `file_entries.owner_id` 和 `deleted_at IS NULL` 做权限过滤，并支持目录及子目录范围。对象内容通过内容寻址存储，多个引用只增加 `file_entries` 关系，不重复保存对象、AI 文档或向量。删除一个引用不会删除仍被其他引用使用的对象和索引；只有对象没有有效引用时，后续清理流程才可回收物理数据。

同一对象的重复索引请求由 `ai_documents` 的唯一约束、任务幂等键和 lease 共同控制。并发请求不会为同一对象产生多份 AI 文档；一个任务正在处理时，其他任务会等待下一次重试或报告处理中状态。

## 7. 观测与阅读顺序

索引和问答均带 OpenTelemetry span。主要业务 span 包括 `ai.index`、`ai.extract`、`ai.chunk`、`ai.embed`、`ai.search`、`ai.search.fts`、`ai.search.vector`、`ai.rrf`、`ai.rerank`、`ai.llm` 和 `ai.ask`，可据此区分耗时来自抽取、数据库、Embedding、重排还是 LLM。

推荐阅读顺序：

1. `backend/internal/ai/service.go`：先看索引状态机、lease 和事务边界。
2. `backend/internal/ai/rag/chunk.go`：理解结构化切块和 citation 元数据。
3. `backend/internal/ai/search.go`：看四种检索模式、RRF、Ask 证据编排。
4. `backend/internal/ai/reranker.go`、`provider.go`：看模型调用和失败降级。
5. `grpcserver/`、`grpcclient/`、`tracing.go`：看部署边界和可观测性。

本轮 RAG 结构化切块与 AI 性能结果见 [`artifacts/ai-eval/20260822T213102Z/PERFORMANCE.md`](artifacts/ai-eval/20260822T213102Z/PERFORMANCE.md)；整个网盘性能结果见 [`artifacts/product-test/20260822T213102Z/PERFORMANCE.md`](artifacts/product-test/20260822T213102Z/PERFORMANCE.md)。
