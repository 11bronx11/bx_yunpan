# AI 独立系统实现与验证记录

日期：2026-11-16。施工基准为根目录 `AI_SYSTEM_SPLIT_CONSTRUCTION_2026-11-16.md`；该施工书保留为需求快照，本文记录实际代码、执行证据和运行边界。原有异步系统说明仍见 `ASYNC_SYSTEM_IMPLEMENTATION_2026-11-16.md`。

## 1. 实际实现

业务 PG 保存文件、权限、对象、配额、AI 请求意图和用户作业；控制 PG 保存唯一的任务执行权威；独立 AI PG 保存派生内容及发布版本。AI 在线进程不创建业务 SQL 池，不跨库 JOIN。公共 API 不创建 AI PG、控制 PG 或 MQ 连接。对象读取和最终权限判断通过 core/v1 RPC 完成。

| 角色 | 依赖与职责 |
|---|---|
| `api` | 业务库、HTTP、AI v2 client；文件名搜索由 core 自己处理 |
| `core-internal` | 业务只读账号；文件/目录/scope/证据回验；任务对象短期读取授权 |
| `async-control` | 业务只读、控制库、MQ；冻结任务、调度、恢复及后继释放 |
| `committer` | 控制库、业务写账号；核心业务效果与 AI 用户作业收尾 |
| `ai-query` | AI 发布视图只读、core RPC、按需模型；无控制库/MQ |
| `ai-content-worker` | parser、core RPC、AI staging；独立 source/text 任务及维护队列 |
| `ai-embedding-worker` | 已发布子文本、embedding、staging |
| `ai-summary-worker` | 已发布规范源块、chat、staging |
| `ai-committer` | 已接纳控制结果、AI 发布账号、receipt、CAS 与墓碑 |
| `ai-text-projector` | AI 发布视图、staging、ES；不调用 Qdrant |
| `ai-vector-projector` | AI 发布视图、staging、Qdrant；不调用 ES |
| `parser-svc` | Go gRPC + Python 子进程；无 PG/MQ |

新增入口集中在 `internal/{aisystem,aiprotocol,aigateway,core}`；角色装配在 `bootstrap/ai_split.go`，账号装配在 `platform/dependencies/ai.go`。`ai.Query/Service`、旧 search projector 等代码保留供历史兼容测试和迁移参考，已退出默认在线装配。`aisvc combined` 明确拒绝启动；旧 indexer 别名只映射 embedding；`search-indexer` 必须显式选择 text/vector 角色。

### 控制接纳与发布

`effect_target` 区分 none/core/AI。AI 结果通知使用独立的 `*.result.ai.NN` 分片，与 core committer 分开消费。worker 只写不可变 staging manifest；控制库接纳后，AI committer 校验 task/generation/input/artifact hash、行数、字节数、输出引用，再在 AI 本地事务发布版本和 receipt。跨库回报丢失时命中同一 receipt，避免二次切 active。

后继在结果接纳时冻结，父结果确认和后继 ensure 在控制库同一事务完成。superseded 结果抑制后继；computed 结果只补发。source/text/vector/summary 分别维护 desired seq 和 active pointer；旧失败快照不能覆盖新成功，旧 source/vector 不能跨越当前依赖版本。墓碑保留同世代不可复活语义。

用户作业由控制库关联任务并汇总，通过 core `ai.job.finish` 提交业务结论；AI 不修改业务作业表。迁移接入旧作业时追加 durable observer，不修改已冻结的输入和预算。无法映射的 processing 旧作业获得 `needs_attention` 结论。

### 独立能力与检索

源提取成功后独立释放文本构建和概览；文本发布后独立释放 embedding 和 ES；向量发布后释放 Qdrant。概览、向量和两种投影各有任务和结果。文本准备不需要模型凭据，fulltext 请求不会调用 embedding/reranker/chat。

AI PG 的规范源块、父片、子片、向量和概览分表。父子片由 `ai/rag/parent_child.go` 确定性生成，父片最多 2400、子片最多 600 个估算 token；child 保存同版本 parent 引用、范围和来源信息，发布时校验范围与实际文本。位置未知时不生成虚假精确偏移。概览读取完整规范源块，按预算分段归并；达到调用/覆盖上限时明确失败或报告覆盖信息，不截前 12000 rune 冒充全文概览。

ES/Qdrant 各自使用隔离 namespace、epoch 和不可变目标。完成证明与 active 版本均由 AI PG 验证。ES 错误/覆盖不完整回退 PG FTS；QD 错误/覆盖不完整回退 pgvector。semantic 必须具有兼容 query embedding，不能靠 PG 回退绕过模型；hybrid 可保留文本分支并报告降级。AI PG 不可用时不绕过证明返回外部索引命中。

scope 在 core 验证并分页后参与召回；多批候选先合并分支排名，再融合。默认最多枚举 10000 个文件，超限明确报错。selected 最多 50 个文件，空 selected 不自动变 all。普通 Ask 证据预算 8000、selected 12000 个估算 token；父片去重并提供文件间软份额，不再固定每文件两片。未进入证据的指定文件在 coverage 中说明。普通 RAG 不代表全文穷尽分析。

模型调用前和返回证据前回验 scope/entry/object generation；回验失败终止请求，不自动全盘重试。引用包含真实 source/content/parent/child ID、文件信息与 excerpt。

### 公共接口与前端

HTTP 路径保留，OpenAPI 和生成物同步：

- `POST /api/v1/search`：name/fulltext/semantic/hybrid；name 不依赖 AI。
- `POST /api/v1/ai/ask`：`scope`、答案、版本引用、coverage、retrieval 降级说明。
- `GET /api/v1/ai/files`：用户权限内 text-ready 文件，服务端搜索、目录/类型过滤、排序、游标分页；有界扫描后允许空页继续。
- `GET /api/v1/files/:id/ai`：source/text/vector/summary 独立 supported/available/freshness/latest_build，askable、preview 信息。
- `POST /api/v1/files/:id/ai/reprocess`：幂等命令；按 targets 重建 source/text/vectors/summary/projection_text/projection_vector。
- `GET /api/v1/ai/jobs/:id`：业务用户作业和能力结论。

React 复用现有 Ant Design/ProductUI。选择器支持草稿、跨页/搜索保留、应用/取消、清空、50 上限和继续扫描；删除/权限失效保留问题和选择并提示修正。文件菜单、概览和上传完成入口可进入指定文件问答。概览 pending/failed 不影响 text-ready 问答；图片缩略图和未知类型的信息/下载保持独立。

### 维护与管理

AI staging、退休版本和未接纳的外部物理输出按宽限窗口维护。维护任务由控制库调度，没有第二套执行租约/重试权威。active、未提交结果、receipt、墓碑和必要清理事实保留。未接纳投影不会在 manifest 清理时丢失物理删除线索。

日志携带 task/request/job/object generation/version，记录计算、receipt 复用、检索分支、降级原因、证据 token 与 partial；各角色有 readiness、metrics 和按能力状态计数。

`async-admin inspect` 支持带世代的 AI object resource，显示 target、后继、作业关联和已接纳输出；`redo` 对停止的 AI 计算/投影任务核实当前对象、引用和业务 intent/profile，续签 grant 后创建一个留痕 intent 子任务。重复命令返回同一子任务，原预算保留；对象失效或已有更新意图时拒绝，改用当前文件重建命令。maintenance/终态快照不作为普通模型任务 redo。

## 2. 迁移与配置

| lane | 当前版本 | 内容 |
|---|---|---|
| business | 36 | 035 请求/意图/作业，036 切换证明；没有自动移动旧表 |
| control | 4 | 003 AI target/后继/作业关联，004 durable job observers |
| AI | 7 | 独立内容与版本、staging、receipt、投影、manifest、清理事实和发布视图 |
| archive | 1 | 显式移动旧 7 表及保存外部 FK 定义；独立 down 恢复 |

AI 三种在线账号分别为 `AI_QUERY_DSN`、`AI_COMPUTE_DSN`、`AI_COMMIT_DSN`。`AI_MIGRATION_DSN` 只给一次性工具；缺失不会回退业务库。query 只读发布视图、不能读 staging，compute 只能插 staging，committer 可发布。`ai-role-init.sql` 初始化这些账号。服务 bearer token 与用户/任务 grant 分开；grant 用 Ed25519、audience 和到期时间约束。TLS/mTLS 配置已接入，实际 smoke 使用内部网络服务 token，没有宣称验证 TLS 证书部署。

`AI_ENABLED`、`AI_VECTORS_ENABLED`、`AI_SUMMARY_ENABLED`、`AI_ASK_ENABLED` 分开控制。embedding revision 和 summary model/revision/prompt 分开版本化。新增键已同步两个 env 模板和 Compose；277 个模板键一致。`init-env.sh` 只补缺项并保留已有配置/密钥。AI 角色不获得业务 DSN/迁移凭据；核心 API 不获得控制库/MQ/AI DB 凭据。

### 新安装

```bash
./deploy/init-env.sh
./deploy/up.sh
# 需要两个外部投影时：
./deploy/up.sh --search-backends --search-indexer
./deploy/status.sh
./deploy/config-check.sh
```

默认启动显式核心和 AI 角色，`--ai-split` 为兼容开关。单独配置 ES 或 QD endpoint 可只启相应 projector。`build.sh` 串行构建，Go 使用 `GOMAXPROCS=2`、`-p=1`；不要并行冷构建全套镜像。

### 保留数据升级

停止旧 AI/search 写者后运行 AI 专用交接，不重跑核心 async-import：

```bash
AI_LEGACY_WRITERS_STOPPED=true ./deploy/ai-upgrade.sh prepare
./deploy/ai-upgrade.sh inspect
./deploy/ai-upgrade.sh verify RUN_UUID
# 重建完成且隔离项已审阅后：
AI_LEGACY_WRITERS_STOPPED=true ./deploy/ai-upgrade.sh archive RUN_UUID
```

工具检查旧 control lease、消费者和消息交接，分批保存 run/manifest/游标/校验摘要。原 attempts/dispatches/recoveries/deadline、成功和停止结论保留；无法证明生效的旧 computed 输出隔离，保留结果而非直接重新调用模型。旧 MQ 消息先持久转移/隔离后 ACK，不 purge。旧 chunks 不伪造父片，默认从原字节重建规范源块和父子文本；不能证明模型来源的旧向量不猜测复用。

`prepare` 可重入；`verify` 仍等待发布时不冒充完成；`archive` 必须通过 cutover-check。归档 down 恢复旧表位置和 FK，不反向生成新输出的旧格式 embedding/summary。服务回滚需停止新发布；旧 query 只能对保留旧数据有限降级，不保证覆盖切换后的新文件。

### 宿主机命令

```bash
./deploy/infra-up.sh
./deploy/local-run.sh migrate up
./deploy/local-run.sh migrate control up
./deploy/local-run.sh migrate ai up
./deploy/local-run.sh reader-init
./deploy/local-run.sh ai-reader-init
./deploy/local-run.sh core-internal
./deploy/local-run.sh ai-query
# 其余角色各用独立终端，名称与上表相同
./deploy/local-run.sh async-admin inspect OBJECT_OR_TASK_UUID
./deploy/local-run.sh async-admin redo TASK_UUID 'provider recovered and current input verified'
```

首次从旧核心异步系统迁移才执行核心 async-import；本轮 AI 工具不代替它。宿主机地址和 metrics 18083–18100 从同一 `.env` 派生，避免与容器争用端口。

## 3. 已执行的验证

证据目录：`artifacts/ai-split-20261116/`（git 忽略；含凭据的测试 env 文件为 0600，不要提交或打印）。

| 检查 | 实际结果与证据 |
|---|---|
| Go build/vet | 最后补修后再次执行 `go build -p=1 ./cmd/...`、`go vet -p=1 ./...` 成功；`backend-delivery-build-vet.log`、`backend-delivery-vet-final.log` 无错误输出 |
| 后端全量 | 最终执行 `go test -json -p=1 ./... -count=1 -timeout 8m`：629 pass、0 fail、6 skip，38 个有测试包通过；`backend-complete-summary.json` / `backend-complete-tests.jsonl` |
| 后续 admin 定向回归 | asynccontrol、asyncjobs、async-admin 编译及测试通过；覆盖续签、去重、原预算保留、新意图/零引用拒绝；`admin-tests.log` |
| 生成物 | 最后再次执行 `make proto` 与 `go generate ./api`，重生成前后 7 个生成文件字节一致；`delivery-generation-check.log` |
| parser | 隔离 venv 执行 `python3 -m unittest parser_worker.test_parsers -v`，17/17；`generation-parser.log` |
| 前端 lint/build | `npm run lint`、`npm run build` 成功；`web-lint.log` / `web-build.log` |
| 前端行为 | 最终全量 21 个套件、126 项全部通过；`web-delivery-tests.log`。目录过滤、跨页草稿、取消/清空、50 上限另见 `web-picker-final.log` |
| 配置静态检查 | 两个模板键一致；两套 Compose 渲染成功；15 个角色凭据/依赖边界及 shell 语法通过；`config-verification.log` |
| 真实跨服务链路 | `TestAISplitRealDependencies` 通过；`fullchain.log`、`browser-fullchain.log` 及全量 JSONL |
| 浏览器 | Chromium 登录、浏览器直传并等待 text-ready、全盘/单/多文件 Ask、版本引用/原文下载、删除后 scope_invalid 保留问题、图片缩略图/预览、未知类型信息/下载；13 项检查、0 pageerror；`browser-results.json` / `browser-trace.zip` |
| 迁移 fixture | 保留旧 summary、分批中断/续跑、原预算/停止结论、真实父子版本、归档 up/down/FK 恢复、历史作业收尾；`migration-tests.log` 和最终 JSONL |

六个 skip：四个 taskqueue 子进程辅助入口直接运行时跳过；`broker_restart_preserves_confirmed_tasks` 未启用破坏性重启开关；`TestRabbitMQStability` 未启用长稳开关。新 AI 核心、真实 MQ/MinIO/ES/QD 链路没有因缺少环境变量跳过。

失败记录保留：`web-final-tests.log` 曾有 4 项在主机繁忙时超过 5/15 秒测试时限，最终全量重跑 126 项通过。`backend-delivery-summary.json` 和 `backend-verified-summary.json` 各记录一次 Redis 50ms 冷握手超时；失败均发生在注入停顿前的连接准备，缓存实际 I/O 预算没有放宽。该 fixture 增加 2 秒有界连接准备重试，故障后的 50ms 预算与 150ms 容差保持；`redis-setup-regression.log` 记录修正后缓存包通过。`backend-env-interrupted-*` 是接续验证时 PATH/etcd 端口未正确装配的中断运行，不作为验收通过证据。

`backend-acceptance-summary.json` 记录资源构建期间另一次全量：590 pass、5 fail、6 skip；失败为中文 parser/ES 链路的父/子测试 3 项及两个 MQ 连接关闭用例。中文链路随后单独通过（`chinese-pipeline-recheck.log`），outbox 包复跑通过。MQ 夹具发现新 AI 队列/结果分片未随旧拓扑一起释放，修正 `testBroker` 为清理全部 WorkQueues 和 core/AI results，且启动失败也登记清理；只在专用测试 broker 上重新检查并删除 1372 个无消息、无消费者的 `bx.async.test.*` 测试队列，另 2 个有状态队列保留（`retired-test-queue-inventory.json` / `retired-test-queue-cleanup-final.json` / `retired-test-queue-cleanup-residual.json`）。未 purge 队列或触碰现有业务栈队列。

上述补修后没有并行镜像构建，最终完整回归 `backend-complete-*` 通过 629 项、0 fail、6 skip，包含此前失败的中文 parser/ES、MQ 与 Redis 用例，以及新增永久暂停队列用例。历史失败日志保留，不用最终通过覆盖它们。

AI 核心用例实际证明了：旧 lease 接纳拒绝与 staging 不可见；AI receipt 成功/控制回报丢失后重放；后继原子释放和重复通知去重；旧 seq/墓碑禁止发布；embedding 和 summary 同时失败后 PG FTS 与文本 Ask 可用；单文件引用超过两片；summary-only 不新增 embedding 调用并处理长文尾部；模型不兼容时 semantic 明确失败、hybrid 降级；权限回验失败不调用 reranker/answer；稀疏资格在第 1001 项仍可找到；未接纳投影的物理清理事实保留。

真实 ES/QD 测试实际使用外部投影完成证明进行检索；分别注入 ES/QD 503，验证 PG FTS/pgvector 回退和另一分支独立可用。真实 upload→entry ready→source→text→vector/summary→ES/QD→Search/Ask→引用及 summary-only 用户作业贯通。现有核心真实上传/校验/安全复制/缩略图/分享/配额/GC 回归通过，并验证 API admission 不依赖健康的 MQ/控制库。

### 专用环境的实际迁移范围

首次全量链路使用专用 PG `127.0.0.1:27432` 上 `async_business`、`async_control`、`ai_split_test_20261116` 三个不同数据库及临时隔离 schema。业务 public 已执行到 36、控制到 4、AI 到 7，均 `dirty=false`，见 `migrations.log`。该实例共享 CPU/IO，只证明独立连接、schema 和角色权限。

MQ 使用测试实例和每次独立 vhost；MinIO 使用测试实例和独立 bucket；etcd 使用测试实例；ES/QD 使用现有真实服务的独立随机 namespace/目标。故障注入通过测试专属代理，不停止已有 ES/QD。模型为确定性 fake provider；没有调用真实付费模型，也没有验证模型回答质量。

保留迁移夹具有 3 个历史对象：1 个登记并重建父子文本、1 个保留旧停止结论、1 个记录不支持；1 份旧 summary 在 archive up/down 后保持原文。旧工作继承 attempts=3、dispatches=4、recoveries=2 和原 deadline。作业 attachment 和无法映射旧作业的 durable 结论另有用例。以上均为专用 fixture；没有批量搬迁现有业务库的历史文件，也没有对现有业务库归档。

## 4. 运行状态与边界

当前代码与契约已在工作区实现；现有业务栈未升级。独立 Compose 项目 `bx-ai-split-smoke-20261116` 已启动新角色，Web 为 `http://127.0.0.1:27480`，配置位于证据目录下 `compose-smoke/deploy/.env`。该专用配置与现有 `deploy/.env`、业务数据卷分开。

专用 Compose 项目的业务、控制、AI PG 使用三个独立容器与数据卷，实际迁移为 36/4/7、均 `dirty=false`（`compose-migrations.json`）；这仍是单机，不表示跨主机或存储资源隔离。etcd、MQ、Redis、MinIO 为该项目独立服务，ES/QD 使用真实服务及该测试专属 namespace。`compose-up-verified.log` 记录启动；`compose-status-verified.log` 记录所有角色 readiness、parser gRPC/metrics、Web/API/storage 及实际环境配置检查通过。

容器验证补修了快速退出的 migration 被 Compose 2.40 `wait` 漏查的问题，改为等待包含已退出容器的 ID 并检查退出码；项目所有权检查支持 `COMPOSE_PROJECT_NAME`。parser metrics 改用镜像自带 Python；旧写者停机标志只由迁移工具读取，在线角色不再随调用环境漂移。

容器业务冒烟还发现迁移交接将旧 `ai/search` 队列置为 `paused_until=infinity` 后，调度器曾尝试扫描到 Go `time.Time`，从而阻塞后续队列。暂停判断现只在 SQL 内完成，预算模型不读取该字段；`TestPermanentlyPausedLegacyQueueDoesNotBlockDispatch` 在真实控制库中验证旧队列保持暂停、当前上传队列继续派发（`retired-queue-regression.log`）。

更新调度器镜像后，`compose-http-verified.log` / `compose-smoke/http-results.json` 记录 8 项真实容器链路通过：两份分片直传、独立 text/vector/summary 发布、真实 ES/QD 命中、ES 完整零命中、三种 scope 的版本引用、summary-only 用户作业。

`compose-smoke/browser-results.json` / `compose-browser.log` 记录新 Web 容器的 Chromium 13 项检查通过、0 pageerror，含新上传 text-ready、单/多/全盘 Ask、删除失效、引用原文下载、图片缩略图和未知类型下载。`compose-smoke/independent-stop-results.json` 记录停止 AI 角色、AI PG、控制 PG 和 MQ 后 API readiness、认证目录读取和 name 搜索继续可用，内容查询明确 503，重启后资格列表恢复；最终状态见 `compose-status-final.log`。该停机验证不证明依赖停机期间异步上传可以完成。

parser 最初的冷建被 Ubuntu/阿里云源未来时间的签名清单阻塞，未关闭 APT 时间或签名校验。后来仅用构建参数指定华为镜像源，在当前源码 Go builder 的二进制基础上，从 Ubuntu 24.04 重新安装系统/Python 依赖；按原 Dockerfile 的运行层步骤构建成功，17/17 parser 测试通过（`parser-runtime-cold-build.log`、`parser-mirror-dates.log`）。当前测试栈已替换为该镜像（`parser-cold-runtime-up.log`）。仓库与用户 `.env` 的默认源保持原值；先前复用缓存运行层的中间镜像不再作为最终运行层构建证据。

真实模型效果、容量、长期稳定性、复杂文档全集、完整随机/故障矩阵、跨主机 HA、容灾、TLS/mTLS 实际部署尚未验证。支持现有文本/代码/配置、文本 PDF、DOCX、CSV、XLSX；不宣称 PPTX 完整解析、旧 DOC/PPT 或扫描件 OCR。图片仍走原缩略图能力。
