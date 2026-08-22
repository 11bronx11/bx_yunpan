# AI/RAG 性能评测

日期：2026-08-22 UTC  
运行态：Docker Compose，3 个 `aisvc` 副本，DashScope Provider，`qwen3-rerank` 开启，Reranker 候选 30、Top-K 8。宿主机为 arm64、4 cores、5.2 GiB RAM，磁盘可用约 8.0 GiB。

## 结果

| 场景 | 负载 | 结果 | 错误 |
|---|---|---:|---:|
| RAG 结构化切块 | Markdown、PDF/DOCX 风格段落、30 节长文档 | 最大 1181 rune；自然句末 100%；长文档约 104.7-111.1 us/op | 0 |
| RAG Markdown 微基准 | 5 次 benchmark | 3.75-4.15 us/op；约 1.6 KiB/op | 0 |
| RAG 段落微基准 | 5 次 benchmark | 3.57-3.65 us/op；约 0.9 KiB/op | 0 |
| Hybrid Search | 12 次，3 VU | 平均 918 ms；P50 651 ms；P95 1.73 s | 0% |
| AI Ask（含 RRF、Reranker、LLM） | 5 次，3 VU | 平均 3.25 s；P50 2.14 s；P95 5.00 s | 0% |

AI HTTP 压测共 17 次请求，全部 HTTP 200；当前 DashScope 外部网络和模型服务时间包含在 Ask/Search 延迟内。该结果不是 Reranker 模型本身的纯吞吐基准，而是用户可见链路性能。

## 辅助质量材料

质量评测不是本报告的性能结论，单独保存在 `accuracy/`：4/4 标注查询 Hit@1，5/5 Ask 的引用合法或无证据拒答。它只用于确认压测请求没有明显返回异常，不代表生产 Recall@K 或模型准确率。

## 可复现命令

```bash
cd backend
go test ./internal/ai/ragtest -run TestReport -count=1 -v
go test ./internal/ai/ragtest -bench=. -benchmem -count=5
go test ./internal/ai/... -count=1
go test -race ./internal/ai/... -count=1
```

真实 HTTP 负载脚本是 `ai-http-performance.js`，运行输出保存在 `ai-http-k6.txt` 和 `ai-http-summary.json`。本次未把 API Key、Bearer Token 或密码写入 artifact。

## 边界

这是当前单机、小数据集、3 副本 aisvc 的性能快照。不能直接与根目录旧 `benchmark.md` 的目录专用压测数值横向比较：旧测试只压一个目录接口、负载模型和运行数据集不同；本轮产品读路径结果见相邻的 `product-test/` 目录。
