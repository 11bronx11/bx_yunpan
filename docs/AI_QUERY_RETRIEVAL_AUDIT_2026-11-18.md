# AI Query / Search / Ask 后端专项复核（2026-11-18）

本轮开始版本为 `d7d3a3e`；拆分前基线为 `1cd68d5`（首次引入 `aisystem` / `aigateway` 的 `de065bd` 的父提交）。已对照 Git 中的 `internal/ai/search.go`、`internal/search/lexical.go`、旧 HTTP 与错误映射，并重读当前 Query、core scope、外部投影读取及角色装配。较早的三份 AI 复核报告只作线索，其通过数字不作本轮验证结果。

范围只涉及后端查询、召回、问答及其 HTTP 契约。本轮没有修改索引计算/发布协议、迁移、前端或用户配置，没有部署新镜像。

## 权限与范围结论

权限下推仍在，不能把本轮发现的目录交集遗漏描述成“所有权限都没有下推”。但该遗漏确实扩大了旧参数组合指定的检索范围，必须修复。

| 边界 | 当前约束 |
| --- | --- |
| HTTP → grant | principal 来自认证中间件；scope 经规范化并携带已提交 content/ACL epoch、有效期与签名 |
| Query → core | `OpenRetrievalScope` 验签、检查 epoch；`StreamScopeMembers` 的业务 SQL 强制 owner、未删除、对象 ready 及文件/目录范围 |
| core → AI PG | 仅采用上述成员的对象、匹配 content generation 和已发布 active content/set |
| PG / ES / Qdrant | PG 按授权 content/set IDs 限定候选；ES 将 content IDs、Qdrant 将 set IDs 写入 filter，在排名前缩小范围 |
| 外部候选 → 正文 | 外部命中重新通过 AI PG 的授权 content IDs 取正文；不完整/范围外命中触发 PG 回退，不能直接作为正文或授权事实 |
| reranker / Answer / 响应 | core 在模型前和响应前回验 grant 与 file/object/generation/entry version；变化返回 `ai.scope_changed` |

旧 HTTP Ask 同时给 `folder_id` 和 `file_ids` 时，旧 SQL 同时使用两个过滤条件。拆分后 handler 的 `else if` 只构造 selected scope，忽略 folder。原有 owner 检查仍存在，但本人的目录外所选文件可能进入模型上下文。该缺陷不以“没有跨用户读取”作为可接受理由。

本轮在 core 业务 SQL 中先解析目录与指定文件的交集，再签发 selected grant。交集包括子目录；空交集或不可访问目录返回 409 `ai.scope_invalid`。重新签名沿用目录 grant 的原始 epoch，防止解析期间移动/撤权后重新取新 epoch 而错误接纳旧交集。显式 `scope` 仍不得混用旧字段。

## 发现并修复的遗漏

| 项目 | 修复前触发与表现 | 本轮修复 |
| --- | --- | --- |
| 文件返回数与片段召回数混用 | 两个文件均相关，第一个文件有多个高分片段；fulltext/semantic `limit=2` 在 PG、ES、QD 上均可能只返回一个文件 | Search 恢复旧版至少 40 个片段的召回池，再按文件聚合并应用 response limit；hybrid 的分支至少 50 项策略保留。召回仍有界，不承诺返回范围内所有匹配文件 |
| PG 连续短语排序 | 大量散落的 `foo ... bar` 可压过完整 `ＦＯＯ bar`；旧实现有 NFKC 连续短语加分与 rank 32 归一化 | 恢复旧 PG 排序表达式；查询去首尾空白，保留原有 canonical AND 匹配条件 |
| ES 连续短语排序 | 拆分后的 ES 查询只保留 canonical AND，丢失旧 `content_normalized` phrase boost | 恢复 `match_phrase` boost 4；必须匹配的 AND 与授权 filter 保留，短语只影响排序 |
| 额度错误跨 RPC 丢失 | provider 的 `AllocationQuota.FreeTierOnly` 在 semantic/Ask 被改成 503；即使返回 ResourceExhausted，新 HTTP 层也一律视为 scope 超预算 | 恢复带 ErrorInfo 的 429 `ai.quota_exhausted`；scope 超预算保持 422。hybrid 有文本时继续降级，并显式报告 `vector.quota_exhausted`。不透出 provider 私有响应 |
| 旧 Ask 交集 | `folder_id + file_ids` 的目录过滤被忽略 | 采用上一节所述的业务 SQL 交集与原 epoch grant；目录外内容不进入 Query |

上述排序和错误恢复不需要新迁移，也不要求本轮重建索引；ES 已有 `content_normalized` 字段。

## 与旧版对应的闭环

| 能力 | 本轮核对结论 |
| --- | --- |
| name / fulltext / semantic / hybrid | 四种 HTTP 模式均有入口。name 仍读 core 业务库；全文不调用 embedding；semantic 需要兼容模型；hybrid 可独立降级 |
| 中文、MIME、目录与逻辑别名 | canonical terms、MIME 过滤、递归目录、同物理对象多个授权逻辑文件映射均保留；新增修复旧 Ask 的组合过滤 |
| 多路排名与 rerank | 按各分支原始分数跨批合并后做 RRF；生产角色装配 reranker 与候选数/top-k 配置；重排失败保留授权候选 |
| 普通 / 指定文件 Ask | 全部、目录、最多 50 个指定文件都有路径；多文件候选份额、父片去重、上下文预算和缺证据 coverage 存在 |
| 引用 | 模型只能选择已给出的证据 ID；返回内容包含 source/content/parent/child 版本及位置。旧“每文件最多两片”限制按新父子片设计取消 |
| 外部搜索后端 | active/coverage 不足、响应错误或范围外候选回退 PG；合法零命中不等于故障；文本/向量分支独立 |
| 缓存、发现与治理 | query embedding cache 含 provider/model/revision；新角色通过 etcd/core RPC；Ask 与付费 embedding 路径没有新增 RPC 重试放大 |

评分数值不属于与旧实现逐值一致的结论：当前单分支内容搜索也返回按排名计算的分数，旧版使用 FTS/cosine 原始分数；name 路径保留排序，但 HTTP score 为 1。本轮恢复了上述实际排序遗漏，保留现有 v2 响应评分形式。旧每文件两片限制与新父子片/coverage 也不作逐字段行为等同。

这些结论表示检查到对应实现及本轮所列场景通过，不代表所有查询质量、任意组合和生产部署全部验收。新模型重建、复杂文档效果、历史数据切换、跨主机 HA 和容量不在本轮结论内。普通 Ask 是有界 RAG，不能当作全文穷尽分析。

## 本轮执行结果

证据目录：`artifacts/ai-query-audit-20261118/`。沿用已存在的专用 parity 业务/控制/AI 测试库；夹具隔离 schema，外部 target 使用本轮随机 namespace。模型与 reranker 使用离线 provider/HTTP 响应夹具，没有使用付费真实模型。

- `reproduced-valid-fixtures-summary.json`：修复前 1 pass / 11 fail / 0 skip，包含父测试；PG/ES/QD 的召回数、PG 短语排序、quota/RPC/HTTP 断言实际失败。
- `scope-reproduced-summary.json`：旧 Ask 目录组合修复前 0 pass / 4 fail / 0 skip，包含父测试；目录外选择、空交集及不存在目录均复现。
- `query-fixed-summary.json`：第一轮修复后 19 pass / 0 fail / 0 skip，包含真实 ES/QD 召回与 ES 短语排名。
- `permission-verified-summary.json`：11 pass / 0 fail / 0 skip；增加混合 owner、交集解析期间移动、Query 前 epoch 变化，以及外部索引违背 filter 返回范围外 child 的断言。范围外正文没有到达 reranker、Answer 或引用。
- `query-packages-summary.json`：355 pass / 0 fail / 1 skip，退出码 0，124.01 秒；包括 aisystem、aigateway、core、ai 全部子包、textsearch、grpcx。唯一 skip 为未启用的真实模型图片专项。
- `role-chain-summary.json`：2 pass / 0 fail / 0 skip，退出码 0，37.41 秒；真实 socket RPC 治理，以及生产角色装配的 HTTP 上传 → 三库/MQ/MinIO/parser/etcd → 内容发布 → ES/QD → Search/Ask/引用与作业链路。模型与 reranker 为离线 provider。
- `query-race-summary.json`：27 pass / 0 fail / 0 skip，退出码 0，28.71 秒；覆盖新召回预算、PG/ES 短语、quota、目录交集、解析期间移动、模型边界回验与范围外外部命中。计数包含父/子测试，不跨轮相加。
- `build-summary.json` / `vet-summary.json`：`go build -p=1 ./cmd/...` 和 `go vet -p=1 ./...` 均退出码 0，分别 191.73 秒 / 14.67 秒。
- `generated-summary.json`：`go generate ./api` 退出码 0，OpenAPI 与生成的 AskRequest 说明同步。`format-check.json`：8 个 Go 文件 gofmt 检查通过。

最初一次复现的两项新夹具有预约后继和 UUID 扫描错误，修正夹具后才记录上述有效复现；日志保留。一次新增权限测试曾发生语法编译错误，修正后重跑；不把该轮算成通过。
首次准备执行 vet 时，验证脚本因测试 Redis 已停止而在读取端口处提前退出，vet 当时尚未启动；脚本改为容忍已停止容器后执行上述成功的 vet，没有为此重新启动测试依赖。

`boundary-check.json` 核对 77 个前端/环境文件，SHA-256 均未变化；相关包测试开始后 backend 源码没有漂移，`git diff --check` 通过。专用 `bx-ai-parity-es-20261118` / `bx-ai-parity-redis-20261118` 已恢复为原来的 exited 状态，容器与数据保留，见 `test-containers-before.json` / `test-containers-after.json`。没有对当前运行应用部署本轮修复，不能把源码与测试结论当作线上已经生效。
