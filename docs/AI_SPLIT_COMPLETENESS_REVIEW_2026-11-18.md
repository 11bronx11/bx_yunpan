# AI 拆分完成情况复核（2026-11-18）

结论：独立数据库、角色、控制结果接纳及本地发布主链已实现；不能把它等同于旧能力全部对齐或现有部署已经验收完成。前两轮发现并修复了真实遗漏，本轮再次发现配置校验、能力隔离、投影确认、作业版本追溯、长摘要恢复和 RPC resolver 并发问题。因此此前“完整拆分”的笼统表述应收紧到具体功能、源码版本和执行证据。

范围保持为 AI 后端。开始时保留上一轮未提交改动；没有修改前端、用户 `.env`、现有应用镜像或现有应用的业务数据。原施工书作为需求基线，旧 `internal/ai` 作为行为基线；此前测试数量不替代本轮结果。

## 本轮进一步发现并修复

| 问题 | 修复前行为与实际复现 | 当前处理 |
| --- | --- | --- |
| 能力之间仍存在配置耦合 | `Computer.Compute` 比较整份 Profile。改 embedding 模型/修订会拒绝冻结的 text/summary/投影，改 summary 会拒绝 embedding；7 个执行用例复现 | 按任务实际使用的依赖比较。文本执行保留的 v1/v2/v3 算法；摘要只比较摘要模型/修订/prompt；向量只比较 embedding 模型/修订/维度；已发布数据投影不依赖在线模型。相关模型身份变化仍在调用模型前拒绝，避免错标来源 |
| 投影写入未确认完成 | ES 写响应带失败 shard 或缺证明、QD 仅 acknowledged/错误正文也可生成 complete manifest；5 个错误响应复现 | ES 要求 created/updated 与明确零失败 shard；QD 同步写要求 status=ok 且 completed。检查 collection 创建结果，复用既有 collection 前验证 1024 维/Cosine。失败时不插成功 staging、不产生发布证明 |
| 新角色跳过旧配置校验 | `Config.Validate` 对 split role 提前返回，未验证 provider、rerank 参数和查询超时。9 个非法配置用例复现；provider 拼错会被 NewProvider 当作 fake | 补回角色相关 provider、超时、候选数/top-k、reranker 模型/地址、搜索后端及外部查询预算校验，保留纯文本准备无需模型密钥 |
| 作业版本追溯只有字段没有链路 | `ai_job_requests.result_version_refs` 原先始终为空；收尾协议不携带版本，业务 committer 不写入，HTTP 不返回 | 控制收尾从已成功结果提取 source/text/vectors/summary 的输出版本；core committer 与结果状态同事务写入；轮询接口返回对应字段并同步 OpenAPI/生成物。增强真实链路断言 summary-only 作业版本等于概览实际发布版本；历史作业不伪造补填 |
| 长摘要缺少分段恢复 | map/reduce 中途失败后，下一次尝试会从第一段重新调用；施工书要求中间摘要也有稳定身份与可恢复工件 | map 与各层 reduce 的完成节点写不可变 staging，身份绑定 task/input digest、source、模型/prompt、算法层次/位置及正文 hash；新执行世代复用已完成节点，仍按整棵计算的逻辑节点预算计数。000010 增加受限 checkpoint 视图，compute 只读该类 staging，query 不可读；复用现有控制证明与有界 GC，不另建调度器 |
| RPC resolver 写进全局注册表 | 后续完整链路运行实际出现 `fatal error: concurrent map writes`，栈定位 `internalClient → resolver.Register`；不是普通断言失败 | 四个 AI/parser 客户端装配入口改用连接专属 `grpc.WithResolvers`；不再覆盖全局 builder，也不借用其他连接将关闭的 etcd client。增加 16 个客户端并发构造与全局注册表保持原值的断言 |

复现证据为 `artifacts/ai-completeness-20261118/reproduced-isolated.log`、`config-reproduced.log`。初次测试夹具存在待领取后继冲突，记录在 `reproduced.log`；修正为独立 schema 与明确关闭夹具后继后重新复现，前一个日志不作为全部问题的证明。作业版本测试初次也碰到夹具后继冲突，修正后在 `job-fixed-2.log` 通过；版本缺失的源码证据为旧收尾协议及旧 committer 未读取/更新该字段。

并发 resolver 崩溃证据保留在 `final-ai.log`；该轮未完成完整测试，不能按其中 `fail=0` 当作成功，应以退出码 1 为准。崩溃使专用 AI 测试库的 vector extension 留在未清理的夹具 schema，后续夹具无法解析 vector 类型；`checkpoint-focused.log` 的这些失败属于环境后果。仅对专用 parity AI 库把已有 extension 迁至 public，没有删除夹具、扩展或用户数据，记录在 `test-extension-recovery.json`。恢复后 `checkpoint-verified.log` 的摘要恢复与完整链路通过。

## 能力、装配与验证清单

下表描述当前实现与验收边界；“有实现”不表示任意数据、任意模型和生产规模均正确。

| 检查面 | 当前实际路径 | 证据/边界 |
| --- | --- | --- |
| AI 与业务 SQL 分离 | API → ai/v2；AI → core/v1；AI 角色 OpenAI，无业务池 | `TestAISplitRealDependencies` 使用独立三库、生产角色和独立账号；业务只读角色不能写 |
| query/compute/commit 权限 | query 读发布视图；compute 仅插 staging；committer 发布与 receipt | 角色初始化与运行时权限检查；真实链路验证。没有用业务全权账号冒充 compute |
| 上传到 AI | 业务事实 → outbox → 控制接管 → source/text 与独立 summary/vector → 各 projector | 完整链路同时包含真实 HTTP、MinIO、MQ、PG、etcd、Python parser、ES/QD；模型为确定性 provider |
| parse | source-only 返回规范块，Go 独立父子构建 | 保留文本、文本 PDF、DOCX、CSV、XLSX 等现有能力；PPTX、旧 DOC/PPT、OCR 未完整支持，不能计作已完成 |
| 图片 | source 准备有视觉提取，与 media 缩略图独立 | provider HTTP 夹具；本轮不重新调用真实付费模型，不延用旧报告的模型效果结论 |
| 中文 FTS | PG/ES 共用 canonical terms，标题加正文 | 实际 PG/ES 正反例、标题召回；迁移 000008/000009 尚未在现有应用库执行 |
| 父子切片 | canonical blocks → parent → child；同版本精确范围、局部 overlap | 覆盖/稳定 ID/重复标题/长文/表格/单列/长末行测试；v1/v2 保留，v3 用于新构建 |
| 标题上下文 | FTS、ES、v3 embedding、rerank、parent evidence | 同一标题+正文问题贯穿各层；原始 child 正文和 parent offsets 保留 |
| embedding | 10 项批量、1024 维、有限数/非零/数量和输入 hash 校验 | 实际 HTTP 夹具、完整每 child 发布；真实模型效果未评估 |
| query embedding/cache | 每次检索复用问题向量；L1+Redis；provider/model/revision/dimension 隔离 | Redis 跨实例测试；disabled vectors 无模型调用；shadow/PG fallback 不多调用模型 |
| RRF | 各分支跨 scope 批次按原分数合并后 `1/(60+rank)` | 共同 rank-2 在 top-1 胜出；201 个版本最后一批高分成为全局第一 |
| rerank | Ask 中候选回验后调用；配置控制整个候选池，再选 parent | 原本存在调用，前轮修复配置遗漏与未重排尾部；开关关闭时使用融合排名。最终真实三库链路显式设置 enabled=true/candidates=3/top-k=2，并要求同一 trace 出现 ai.rerank，证明角色装配实际调用了离线 reranker；HTTP provider 协议有测试，真实 reranker 质量未验收 |
| top-k 与上下文 | 分支召回数、rerank 候选数、答案 parent 数、token 预算分开 | 当前配置默认候选 30/top-k 8；普通硬上限 12 parent/8000 tokens，selected 24/12000；不是保证给 50 个选中文件各一片 |
| 普通 Ask | 当前有权且 text-ready 范围，多路检索、软文件多样性 | 无向量仍可文本问答；无证据不发 Answer；普通 RAG 非全文穷尽分析 |
| 指定单/多文件 | core 先验最多 50 个 ID，范围内召回，不扩大范围 | 无每文件两片硬限；相关文件机会、alias、parent 去重、长文 token、缺失证据 coverage 测试 |
| 模型前后权限 | reranker/Answer 前与返回前 core 回验 | 错 owner、过期/伪造 grant、删除/世代/ACL 变化、未知引用用例；失败不全盘重试 |
| 概览与资格列表 | source/text/vector/summary 状态独立；分页扫描 text-ready 文件 | vector/summary 失败不挡资格；稀疏合格项跨多页；概览使用已发布模型与 pipeline 元数据 |
| 摘要 | 完整 source 分段、逐层 reduce、调用预算、partial 来源继承；各层完成节点持久恢复 | 尾部标记、多层汇总及真实控制世代重试验证；重建 computer/provider 后不重复已完成段，拒绝损坏 checkpoint，新输入身份不复用旧节点，GC 等控制终态证明。模型成功但 checkpoint 落库前中断仍可能重复调用，不承诺外部恰好一次 |
| 能力升级隔离 | 本轮按实际模型/算法依赖校验冻结任务 | 无关升级继续执行，相关模型不匹配不调用 provider；需要旧模型的冻结工作不会自动改标为新模型 |
| staging/接纳/发布 | staging → control 接纳 → AI 本地事务版本/receipt → control Report | manifest 篡改、旧世代、重复 receipt、SIGKILL/ACK 故障窗口；computed 只补发 |
| ES/QD 投影与回退 | 不可变目标、coverage/active 证明；分支独立，失败回 PG | 实际 ES/QD 加故障代理；零命中与错误分开；本轮进一步校验写完成与 QD geometry |
| 删除/退休/维护 | 墓碑与宽限期；控制调度；外部完成后才忘记清理事实 | 部分删除响应拒绝、孤儿投影保留、GC 跳过受保护行后有界前进 |
| 用户作业 | 控制聚合，core 本地事务收尾 | summary-only、partial_success、历史 observer；本轮补输出版本引用。历史未记录版本不会自动恢复 |
| RPC/发现 | 连接专属 etcd resolver、round_robin、服务认证、scope/processing grant、有限 deadline/重试 | 实际 socket 治理与并发连接构造；Ask/embedding/parser 不因只读重试放大付费调用。TLS 配置存在，但本轮真实链路不等于 mTLS 部署验收 |
| 开关与核心独立性 | AI 关闭时内容接口 503，name/core 不依赖 AI PG/MQ/query | 16 种能力开关与无 query 的命令测试；新增 split-role 非法配置测试 |
| 旧数据与旧任务迁移 | 独立 prepare/verify/archive，保预算/终态/消息 | 真实旧 schema、205 对象中断续传及归档夹具；尚未对现有应用执行完整升级切换 |
| 部署 | 启动文件已有显式 AI 角色 | 本轮读取现有镜像为 `upload-window-fec24fa-20261117`，健康只说明探活通过；不包含本轮工作区修复。未执行部署 |

当前运行状态另有直接证据：只读提取 `deploy/.env` 与运行中的 `bx-yunpan-ai-query-1` 白名单开关，两者均为 **AI_RERANK_ENABLED=false**，候选数/top-k 均为 30/8；因此现有部署本来就不会调用 reranker。开关关闭和源码装配遗漏是两个已核实的事实。本轮修复了开启后的执行路径，并用显式 enabled=true 的真实角色链路验证，但保留用户配置，没有擅自开启当前环境的模型调用。安全过滤后的记录为 `runtime-ai-settings.json`，不包含凭据。

## 本轮执行记录

证据根目录 `artifacts/ai-completeness-20261118/`。继续使用专用 parity 三库，每项集成夹具隔离 schema，完整链路另隔离 vhost/bucket/投影 namespace。私有环境文件权限 0600；不从用户 `.env` 读取模型凭据调用真实服务。

| 执行项 | 实际结果 |
| --- | --- |
| 修复前隔离复现 | 2 pass / 14 fail，失败含父测试；7 个无关配置耦合与 5 个错误投影响应的断言失败 |
| 修复前配置复现 | 0 pass / 10 fail，含父测试；9 个非法配置全部被接受 |
| 第一轮定向回归 | `isolated-regression-summary.json`：60 pass / 0 fail / 0 skip；后续作业版本补齐另测 |
| 作业版本定向 | `job-fixed-2-summary.json`：1 pass / 0 fail / 0 skip |
| 后续首轮全量 | `all-summary.json`：851 pass / 2 fail / 7 skip，退出码 1；AI shadow 夹具还用空响应模拟写成功，已修正；另一个失败为未改动的 upload `TestBatchConfirmationAndFrozenReplay` 时间戳相等断言。此轮不记为全量通过 |
| 后续定向与崩溃 | `final-ai-summary.json` 退出码 1，386 pass / 0 个已结束的失败测试 / 1 skip，但 bootstrap 因 resolver 并发写崩溃；不能看 test 数量忽略进程失败 |
| 摘要恢复与 resolver 定向 | `checkpoint-verified-summary.json`：7 pass / 0 fail / 0 skip，含真实三库主链；后续完整验证还包括新增 checkpoint GC 断言 |
| 补齐恢复后的全后端 | `verified-all-summary.json`：854 pass / 3 fail / 7 skip，退出码 1；3 fail 为同一个能力隔离测试的两个摘要子用例及父测试。旧断言把 checkpoint 也当成最终结果而期待总数 1，已改为按最终任务 kind 计数；此轮上传时间戳用例通过。其余已执行的包通过，随后重跑全部受影响 AI 包 |
| 最终 AI 相关包全量 | `verified-ai-summary.json`：407 pass / 0 fail / 1 skip，退出码 0，155.66 秒；覆盖 aisystem、bootstrap、config、dependencies、registry、grpcx、aigateway、aiprotocol、asyncjobs、ai 全部子包。skip 为真实模型图片专项，未把未调用的真实模型计为验收 |
| 定向 race | `verified-race-summary.json`：81 pass / 0 fail / 0 skip，退出码 0，103.49 秒；覆盖排名、Ask/selected、父子片、摘要恢复与 GC、投影响应、能力隔离、作业版本、RPC 并发及完整链路 |
| 显式启用 rerank 的最终主链 | `verified-rerank-flow-summary.json`：1 pass / 0 fail / 0 skip，退出码 0，34.47 秒；真实三库/角色/HTTP/MQ/MinIO/parser/etcd/ES/QD，模型与 reranker 为离线 provider，并启用 race。加强完整链路 rerank 断言时，上一轮 race 的源清单只有该测试文件发生变化，生产源码未变；此项用最终冻结测试源码单独重跑 |
| Python parser | `parser.log`：17 项通过，退出码 0；Python AST 检查 3 个改动文件通过 |
| OpenAPI 生成物 | `go generate ./api` 退出码 0；生成代码重新生成前后 SHA-256 相同 |
| build/vet | `go build -p=1 ./cmd/...` 与 `go vet -p=1 ./...` 均退出码 0，分别 121.89 秒/7.37 秒；构建开始到最终检查，backend/deploy 的源码及测试哈希没有变化 |
| 修改边界 | 76 个前端文件 SHA-256 不变，git 无前端改动；deploy/.env SHA-256 不变；现有 26 个运行应用容器名称/镜像不变；35 个 Go 文件 gofmt 与 git diff --check 通过 |

所有数字含 Go 子测试，不能当作独立业务场景数量；不同轮不相加。最终日志与 `*-source.json` 对应执行时源码，不拿较早的通过结果替代后来改动。

依赖验证完成后，专用 ES `bx-ai-parity-es-20261118` 与 Redis `bx-ai-parity-redis-20261118` 已恢复为停止状态，容器和数据保留。专用 parity 三库、崩溃遗留夹具及复核证据保留，没有 purge 消息或删除卷；状态见 `owned-resources.json`。`boundary-check.json`、`runtime-boundary.json` 与 `final-source-consistency.json` 记录最后一次修改边界核对。

## 完成边界

“源码具备独立运行结构”“所列回归通过”“模型效果达标”“现有部署已完成切换”是四个不同结论。目前只对实际检查和执行的前两项给证据。真实 reranker/问答质量、复杂真实文档评测、现有数据升级/重建/发布，以及跨主机 HA/容量，不计作本轮已完成。

此前修复的 000009 及本轮 000010 仍待现有环境迁移；000010 后还需执行更新后的 `ai-reader-init` 授予 compute checkpoint 视图只读权限。新切片与标题向量还需显式重建。本轮未自动批量重建、未改前端、未部署或清理现有数据。不能据本报告把整个 AI 拆分标成“全部验收完成”。
