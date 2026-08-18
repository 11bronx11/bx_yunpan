# BX YunPan 本机性能基线

- 日期：2026-08-17
- 环境：4 vCPU、5.2 GiB 内存、Docker Compose，本地 MinIO/PostgreSQL 17/Redis/etcd
- Provider：`AI_PROVIDER=fake`，不包含外部模型网络波动
- 数据集：测试结束时 52 个 active 文件、52 个 AI 文档、629 个 AI chunk，数据库约 19 MiB
- 结论边界：这是小数据集上的 smoke baseline，用于比较本轮改动前后的方向，不是生产容量承诺。没有做多机、长时间稳定性或故障恢复容量结论。

## 结果

| 场景 | 配置 | 结果 | 错误 |
|---|---|---:|---:|
| 目录元数据读 | k6，20 VU，20s，69,503 次 `GET /folders/:id/children` | 3,472.86 req/s；平均 5.69ms；P95 22.32ms；P99 38.74ms | 0% |
| 8 MiB Multipart 全链路 | 预签名、MinIO PUT、分片确认、完成、Worker SHA-256 校验 | 0.964s 端到端；8.30 MiB/s | 0 |
| 检索 span 拆解 | 25 条 hybrid Search，1 个 aisvc，OTel/Jaeger | FTS P50/P95 1.828/6.744ms；Vector 2.200/9.172ms；RRF 0.041/0.087ms | 0 |
| AI backlog drain | 24 个文本文件先排队，再启动 aisvc | 1 副本：23.563s，1.02 docs/s；2 副本：15.245s，1.57 docs/s | 0 |

2 副本相对 1 副本约提升 `1.54x`。这里的计时包含启动 aisvc 并等待 backlog 清空，且 PostgreSQL、MinIO 和 fake Provider 仍是共享瓶颈，不能外推为线性扩容结论。

## 向量索引参数

`idx_ai_chunks_embedding` 使用 pgvector HNSW，迁移没有设置 reloptions，因此使用扩展默认值：`m=16`、`ef_construction=64`。本次数据库会话中的 `hnsw.ef_search` 为 `40`。数据量只有 629 个 chunk，没有进行有统计意义的 Recall@K 调参；后续应使用固定标注集对 `ef_search` 做召回率/P99 对比。

## Trace 证据

Jaeger 中已确认以下跨进程链路：

- 上传完成 trace 同时包含 `bx-yunpan-api`、`bx-yunpan-worker`、`bx-yunpan-aisvc`，并覆盖 `object.verify_requested`、`object.ready`、`ai.index`、`ai.extract`、`ai.embed`。
- Search trace 包含 `yunpan.ai.v1.AIService/Search`、`ai.search.fts`、`ai.search.vector` 和 `ai.rrf`。
- Ask trace 包含 `yunpan.ai.v1.AIService/Ask`、检索四段和 `ai.llm`。

上传链路截图：[Jaeger upload trace](assets/jaeger-upload-trace.png)。

## 重跑方式

```bash
./deploy/up.sh --no-build
./deploy/status.sh
```

性能测试需要在 `AI_PROVIDER=fake` 下单独运行，并在报告中记录 CPU、内存、数据量、aisvc 副本数和是否启用 OTel。检索延迟应从 Jaeger 的 `ai.search.fts`、`ai.search.vector`、`ai.rrf` span 聚合，而不是只测 HTTP 总耗时。
