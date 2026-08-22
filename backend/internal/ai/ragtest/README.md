# RAG 轻量评测

这个目录只做本轮 RAG 改动的可重复对比，不是完整压测，也不调用外部模型。

运行：

```bash
cd backend
go test ./internal/ai/ragtest -run TestReport -count=1 -v
go test ./internal/ai/ragtest -bench=. -benchmem -count=5
```

固定样本覆盖 Markdown 标题/段落和 PDF/DOCX 风格段落。结果重点看：

- chunk 数量、平均/最大 rune 数；
- chunk 结束在句末等自然边界的比例；
- Hybrid 每一路初始召回数与 RRF 最终保留数。

`results/` 保存同一环境、同一 fixture 的改动前后输出。它们只用于说明本轮变化方向，不能当作生产容量或通用 Recall@K 结论。
