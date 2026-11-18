# AI 拆分能力补齐与实测（2026-11-18）

本轮针对拆分后遗失的既有能力与关键 AI 行为做源码对照、修复和重新执行验证。基线为 `d9c5e568228bd56affc8646ac7ddfa6349c05935`。旧实现与施工书用于寻找差异，旧验证报告不作为通过证据。

**前端没有改动。** 已复核开始时记录的 76 个前端文件 SHA-256，全部相同；未运行前端构建。现有 `deploy/.env`、运行中的应用、原 ES 配置和用户数据未修改。以下代码尚未部署。

## 遗漏、错误与修复

| 项目 | 实际断点 | 本轮处理 |
| --- | --- | --- |
| 图片视觉链路 | 旧 `ai/service.go` 有视觉提取，新 core/planner 只接受文档，Computer 全部送 parser | 恢复 PNG/JPEG 的视觉提取，经 source → text → embedding / summary 发布；保留视觉模型来源信息、对象哈希/大小校验、像素限制 |
| 图片概览 | 图片分支隐藏已发布摘要；无缩略图证明也返回 available | 图片摘要独立返回；由 core 的业务只读查询返回实际缩略图状态 |
| 模型身份 | 冻结 Profile 不包含视觉模型 | 新任务冻结 provider/model；已冻结旧文档任务保留兼容执行路径 |
| 中文 PG 搜索 | 新 AI 库把 bigram 用 OR 拼接，单字漏召回，连续词大量误召回 | AI 迁移 000008 恢复与旧 `textsearch` 相同的 NFKC、中文单字/bigram、AND 查询契约，回填已有子片 tsv，不重跑 embedding |
| 中文 ES 搜索 | 新索引退回标准分词，未接入原 canonical terms | 恢复 `content_search` whitespace mapping 和版本标记；使用新 schema digest 隔离旧索引；旧 computed 投影仍可接纳，reader 对不兼容版本回退 PG |
| ES 重复投影 | 查询已存在 mapping 时 GET 带 JSON null，第二份文档持续失败 | nil 请求体不再发送；幂等创建后验证实际 mapping；检查超时、失败分片、缺失结果结构 |
| 查询向量缓存 | 新 Query 没有装配原 L1 + Redis 缓存 | 恢复两级缓存；key 包含 provider/model/revision/dimension/归一化；缓存异常回源，IO 有独立预算；真实 Redis 跨实例命中已验证 |
| 向量与响应边界 | 禁用向量仍可能调用模型；模型版本混合会拖垮全部结果；embedding index 未严格验证 | 开关先行；兼容集合继续检索并明确降级；拒绝重复/缺失索引、错误维度、非有限数和零向量；新向量正规化 |
| 外部后端配置 | read backend、write backends、shadow 配置未完整装配 | 恢复 PG / ES-QD 读选择、冻结投影写选择；shadow 复用同一授权范围和查询向量，只记录数量与摘要，不追加模型请求；删除/退休继续处理既有外部数据 |
| 文件名搜索 | 旧 exact/prefix/fuzzy 排序退化为子串加名称排序；省略 include_subfolders 改成 false | 恢复模糊召回与排序，以及默认递归；AI 不可用时仍走业务 PG |
| 父子切片 | 每个小段落单独成父片；表格丢失表头与行组语义；长前缀重复估算成本过高 | 新 `parent-child-v2` 合并同章节、同位置的相邻段落/列表；表格按行组保留表头与工作表/行列范围；增量 token 估算；保留 v1 的 ID、空块序号与执行逻辑 |
| 引用来源 | 新 RPC 只保留页码和 section，工作表、行列、编码等来源信息未返回 | 增加可选 `source_location`，保留原字段；OpenAPI 与 Go 生成物同步；不伪造字符精度 |
| 长文摘要 | 只有一层 reduce，多段摘要合并超过预算即失败；预算错误还会重复消耗任务尝试 | 改为逐层完整归并，共用调用预算；预算耗尽永久收敛，不静默丢尾；单段不额外 reduce；混合空白 PDF 页标注部分覆盖 |
| 能力状态 | 尚无 AI object 时跳过开关；source 失败只终结 source，下游长期 pending | 补齐禁用状态与失败后继；按各能力 seq 更新，保留已有成功版本 |
| 物理对象别名 | 多个合法 entry 被 uniqueVersions 压成一个，搜索和所选文件引用遗漏 | 召回内容去重，保留所有授权逻辑文件映射；RAG 证据正文去重；权限回验分批，防止别名数量突破 core 的 RPC 上限 |
| Grounded 结果 | 只要模型给引用就可能被 HTTP 当成 grounded | 只有模型明确 grounded 且引用属于已验证证据时返回引用 |
| HTTP 数字契约 | protobuf JSON 把 int64 版本/偏移改成字符串 | 保留现有 HTTP 数字类型，并用 json.Number 避免适配时损失整数精度 |
| gRPC 治理 | 新内部客户端/服务端主要只保留认证 | 恢复总 deadline、按方法及模式隔离的熔断、request ID、metrics、panic recovery、具体 service health；只对无模型只读调用重试，Ask/semantic/hybrid/parser 不追加 RPC 重试 |
| OTel | 新角色未初始化 provider；RPC、模型 HTTP、后继任务和发布阶段传播缺口 | 补齐角色初始化/flush、client/server handler、后继 trace、AI commit 恢复、解析桥接和模型 HTTP span；增加准备、检索、重排、回答、投影等阶段 span |
| parser TLS 探活 | healthcheck 固定用明文，且未检查 SERVING 值 | 使用与服务一致的 TLS transport，并检查具体服务的 SERVING 状态 |
| 当前视觉模型配置 | 实际模型列表不再包含 qwen-vl-plus；请求返回 model_not_found / 404 | 仓库默认值及两个模板改为实际可用的 qwen3-vl-plus；模型输出设有预算并关闭额外 thinking；原运行 .env 保留，尚未切换 |

主要入口为 `backend/internal/aisystem/{compute,planner,publish,projection,query}.go`、`ai/rag/parent_child.go`、`platform/grpcx/client.go` 和 `bootstrap/ai_split.go`。新增验证集中在 `aisystem/*parity*test.go`、`real_models_test.go`、`bootstrap/ai_rpc_parity_test.go` 和增强后的完整链路测试。

## 原计划的收紧继续保留

独立 AI PG、compute 只写 staging、ai-committer 才发布、查询只读发布视图、core scope / processing grant、召回前范围确定、模型前与返回前权限再验、computed 结果不重算、有限任务预算，均属于施工书要求。本轮没有恢复业务 SQL 直连、跨库 JOIN、第二套任务权威或未授权全局召回。

父子片 v2 是新构建版本；v1 保持可执行，已接纳结果按 receipt / manifest 规则发布。新 profile 的视觉身份字段为空时不会改变旧 JSON 的摘要，旧文档计算只对明确保留的 v1 实现和缺失视觉字段做兼容。

PPTX、旧 DOC/PPT 和扫描 PDF OCR 并非本次拆分前已完整支持的能力，没有冒充回归遗漏补齐。普通 RAG 仍不是全文穷尽分析；超过预算明确失败或报告覆盖不足。

## 实际验证与边界

验证环境使用独立业务、控制、AI 测试库，每个夹具进一步隔离 schema、MQ vhost、MinIO bucket 和投影命名空间。Go 使用 1.25.13、GOMAXPROCS=2、-p=1。没有把全量集成测试指向开发业务库。

| 验证 | 结果 / 证明范围 |
| --- | --- |
| 全后端 go test ./...，启用真实依赖及真实模型专项 | 最终核验轮通过：790 pass / 6 skip / 0 fail，退出码 0；耗时 443.64 秒，计数含子测试 |
| 强化后的 HTTP 全链 | 通过：上传 → MinIO → RabbitMQ → 三库 → parser → 发布 → ES/QD → Search/Ask → 引用 → summary-only 重建；模型使用 deterministic provider |
| PG / ES 中文同样例 | 真实 SQL、真实 ES 和 HTTP 正反例通过：单字、连续词、非连续误拼接、多个不相关词；实际注入 ES/QD 独立故障并验证各自回退 PG |
| 实际模型图片链路 | 直连通过：qwen3-vl-plus 识别生成图片的 BX7319 → AI PG 发布 → 实际 document embedding、summary、query embedding、grounded answer 与版本引用；聊天 qwen-plus，向量 qwen3.7-text-embedding |
| 实际模型验证限制 | core 授权与对象 HTTP 在该专项中为夹具；真实 core / MinIO / MQ / etcd 的生产装配由另一项完整链路测试验证。实际 reranker 未启用，因此不声称真实 rerank 效果已验证 |
| OTel 完整链 | 通过：HTTP 根 trace 经过 outbox / 控制后继、source、text、vector、summary、publish、Search、Ask；使用 SDK exporter 断言同一 trace。另有真实 socket RPC 的 request ID、重试、panic、deadline、认证和 metrics 测试 |
| Python parser | 17 项通过，包含实际文本编码、PDF 文本层、DOCX 结构、CSV/XLSX、损坏/不支持输入与 HTTP 请求上限；没有 OCR 完整性结论 |
| 切片 / 摘要边界 | 5 个固定 seed × 100 份混合文本属性用例、长文本末尾、父子 offset、同章节分组、表格末行与引用行号、冻结 v1 空块 ID、分层摘要及预算停止 |
| 缓存与开关 | L1 命中、修订隔离、缓存不可用回源、真实 Redis 跨 Query 实例命中；disabled vectors 不调用模型，hybrid 文本降级 |
| 竞争与故障 | 全套既有 computed / commit / ACK 故障窗口、生产循环 SIGKILL 和权限变更用例随全量通过；最终源码定向 race 9 项通过，0 fail / 0 skip |
| build / vet | 最终源码 go build ./cmd/... 和 go vet ./... 均通过，退出码均为 0；耗时分别为 272.98 秒和 10.21 秒 |
| 生成物 / 格式 / 配置 | Proto 与 OpenAPI 重新生成后哈希稳定；45 个改动 Go 文件 gofmt 检查通过；两份环境模板键集合一致；git diff --check 通过 |

证据保存在 `artifacts/ai-parity-20261118/`，最终通过轮位于 **`verified/`**。`final/` 中前一轮有一项 gRPC 启动探活失败，保留失败证据；修正连接就绪等待后，该治理测试连续 10 次通过，并在最终全量和 race 轮通过。先前探活在连接尚未就绪时密集重试，可能触发健康方法的熔断；没有把这次失败从记录中删除。

`deploy/tests/ai-parity-validation.py` 记录命令、源码 SHA-256 清单、结果日志及按用例计数；pass 数包含 Go 的子测试，不能当作独立业务场景数量。6 项 skip 包括 4 个由父测试单独调用的子进程辅助入口，以及未提供独立 RabbitMQ 容器的 broker 重启场景和未设置时长的长时稳定性测试；后两者不计为已验证。最终全量已实际执行真实模型图片专项与独立 Redis 的故障注入，并非缺少变量而跳过。

真实模型前期出现过网络超时；原模型的 404 已由模型列表及实际请求明确复现。临时本机转发的试验也失败过，不计为通过证据。最终通过使用 Go provider 直接访问真实服务；不把先前对 TLS 的推测作为已确认根因。

## 部署、迁移和运行状态

- 代码、proto、OpenAPI 和默认模板已修改，尚未提交或部署；没有重启现有服务。
- AI 迁移新增 **000008**，只在本轮独立测试 schema 验证。它回填全文索引数据，不重跑解析或向量模型；生产尚未执行。
- 新 ES schema 使用不同 target digest。旧 target 不满足新 lexical 版本时读 PG；新 target 只有完成发布覆盖证明才可接管。
- 新切片策略通过新构建生效，现有已发布 v1 结果保留。没有自动重建全部用户对象。
- 原 `deploy/.env` 的 AI_VISION_MODEL 仍为失效旧值，当前运行二进制也尚未包含本轮修复；不能据测试结果宣称线上图片能力已恢复。
- 原 ES 因磁盘水位拒绝新分片。本轮未修改其水位或删除用户索引，验证使用独立临时 ES；这不代表原 ES 运行容量问题已解决。
- 实际运行的 OTEL_TRACES_ENABLED 保持原值。SDK 的链路验证不等于现有 Jaeger / OTLP 部署已经接收这些新角色的 trace。
- 跨主机 HA、生产容量、任意文档/图片的模型准确率均不在本轮实测结论内。

## 本轮临时资源

专用数据库 `parity_20261118_business`、`parity_20261118_control`、`parity_20261118_ai` 和只读账号 `parity_20261118_reader` 为本轮创建，保留供复核。测试夹具负责回收自己的 schema / vhost / bucket / 投影。独立 ES `bx-ai-parity-es-20261118` 和独立 Redis `bx-ai-parity-redis-20261118` 在全部依赖验证结束后停止，保留容器及其数据供复核；没有删除现有应用容器、数据库、卷或缓存。最终状态见同目录 `owned-resources.json`。
