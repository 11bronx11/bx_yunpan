# AI 独立系统深入测试执行记录（2026-11-17 计划）

结论：**部分完成，未达到计划第 15 节完整验收门槛。** 84 组中 22 PASS、0 未解决整组 FAIL、2 BLOCKED、60 NOT_RUN。NOT_RUN 包含已验证子集但缺指定矩阵的场景。P0 仍有 29 组、P1 仍有 30 组未完整覆盖。

计划日期为北京时间 2026-11-17，本轮宿主机实际时钟为 2026-11-16，原始日志保留真实 UTC 时间，未修改系统时间。

## 1. 被测版本与环境

初始 HEAD 为 `de065bd8eb8fb1ab89d40c086372ddb3b2a9bd2a`，工作区初始干净。本轮代码、测试、runner 和报告均未提交。最终全量输入快照：`2a75d52114b26166145538443d122335f19843a80b5f39981e2cecfc861bf1ec`，见 `manifest/all-final-source.json`；报告生成前确认 backend/web 输入无漂移。后续修正部署 runner 的 login 字段并增加停机期间下载字节核对，均已重跑 E2；最终 runner 快照另见 `manifest/execution-final-source.json`。

证据根目录为 `artifacts/ai-deep-validation-20261116T145543+0800/`，凭据、原始日志和浏览器夹具受保护且被 Git 忽略。报告仅记录脱敏结论，不公开 DSN、token、私钥、签名 URL 或模型 Key。

| 层级 | 本轮实际环境 | 边界 |
|---|---|---|
| E0 | 确定性 fake provider、组件屏障、Unicode 性质测试 | 无付费模型效果结论 |
| E1 | 专用迁移后三库、MQ、MinIO、Redis、etcd、真实 parser/ES/Qdrant | 三库共享一个 PG 实例，不能证明资源隔离 |
| E2 | `bx-ai-split-smoke-20261116`，Web/Nginx `127.0.0.1:27480` | 三 PG 容器及 system identifier 不同；36/4/7、dirty=false；仅单宿主机 |

角色权限探针 27/27 通过；实际 TCP 目的地快照符合角色边界，见 `manifest/roles-final/role-probes.json`、`manifest/socket-destinations-final.json`。8 个角色的显式连接池、并发、租约、批次及预算环境值保存在 `manifest/runtime-budgets.json`；连接快照不等于持续网络审计。

宿主机默认 Go 1.25.7 低于 AGENTS.md 要求，前期结果保留。最终构建/后端回归与镜像显式使用缓存中的 Go 1.25.13；工具链记录随阶段摘要保存。Node 为 22.23.0、Python 为 3.12.3。

## 2. 确认并修复的五个问题

| 问题 | 修复及验证 |
|---|---|
| 最后引用归零后，旧 processing grant 仍能获取新读 URL | core 查询增加 `reference_count>0`；正常引用成功、零引用及旧世代拒绝。真实 MinIO 证实既发 URL 在 TTL 内仍可读，到期返回 403，授权截止不晚于 grant。 |
| 空白摘要可被记录为成功 | map/reduce 每次响应和发布入口均拒绝空白；发布同时拒绝零 source block 的成功摘要。坏向量/数量/NaN/provider error 不产生成功 manifest。 |
| 零引用历史对象被误标为 queued | plan/enqueue 记录 `skip_unreferenced`；enqueue 重查事实，cutover-check 接受明确跳过结论。205 对象五类各 41，只有 41 个 intent。 |
| 前 100 个受保护 artifact 遮挡第 101 个可删项 | GC receipt 原子记录 keyset 游标，下一批继续，末批回绕复查；控制库仍为唯一调度权威。第 101 项回收，受保护 100 项保留。无新增迁移。 |
| 越权 reprocess 返回 409，与 OpenAPI 的 404 不一致 | 不存在或无权限文件返回 NotFound；其他 DB 错误保留。确认无 job/request 写入，真实 HTTP 边界 24/24 复验。 |

另修正既有选择器测试的 4 处 lint 违规，改用可访问性查询打开/展开目录，产品组件无改动。浏览器 runner 支持可选 `AI_CHROMIUM_EXECUTABLE`。新增测试在 serviceauth、core、dependencies、config、aiprotocol、aisystem、asyncjobs、ai/rag；测试屏障和 AMQP 代理没有进入生产调度。

## 3. 已执行证据

Go 1.25.13 最终全量：**741 pass / 0 fail / 6 skip**，顶层 447 pass / 0 fail / 5 skip。`toolchain-build`、`toolchain-vet`、`toolchain-race` 及 `all-final` 摘要保存实际工具链和命令。无测试文件 package 36 个，另计。

| 验证 | 已执行结果/证据 |
|---|---|
| 数据库最小权限 | 27/27，关闭只读会话默认参数后仍由 GRANT 拒绝越权 |
| 真实三库至 Ask | `TestAISplitRealDependencies` 顶层明确 PASS，零 skip；`fullchain-attempt-2-summary.json` |
| 实际 Web/Nginx Chromium | 13 项 PASS，`page_errors=[]`；文本/PNG/未知二进制上传、scope、引用/原文下载、缩略图和 SHA |
| HTTP 权限及参数边界 | 24/24；匿名六端点、跨 owner、非法参数、65535/65536/65537 字节、超长问题 |
| 实际镜像 mTLS | 14 项含子测试检查通过；临时 core 容器合法 RPC、错 CA/SAN、过期/缺客户端、明文、证钥错配启动失败 |
| 崩溃屏障 | AI 事务 pre-receipt、AI commit/pre-Report、生产 worker SaveResult/pre-ACK、生产 committer Report/pre-ACK 的真实 SIGKILL |
| ACK 后恢复不变量 | 拦实际 Basic.Ack 帧后杀子进程；broker 重投无 compute 再调用、不增加 attempts，receipt/source/后继唯一 |
| 迁移进程恢复 | plan/handoff/enqueue 子进程批次提交后 SIGKILL，同 run 恢复；100/200/205 checkpoint，205 分类账目守恒 |
| 旧 MQ 消息 | 独占新 vhost 内 5 类实际旧消息；持久写失败不 ACK，重入 5 条唯一隔离事实，core sentinel 保留；COMMIT/pre-ACK 杀工具尚缺 |
| parser | 17 项通过 |
| 有界 fuzz | BackoffBounds / DigestRoundTrip 各 15 秒、两个并行 fuzz worker，通过；记录实际 seed corpus 与执行次数，无长期压测结论 |
| 前端 | lint、21 套件/126 项、production build 均通过 |
| 生成物/配置 | 隔离副本 proto+OpenAPI 生成成功，7 文件逐字节不变；模板 277 键一致、26 shell 语法通过、隔离 Compose render 成功 |

用户 `deploy/.env` 的前后 SHA 一致。没有长稳/浸泡、峰值 QPS、生产容量、跨主机 HA 或真实模型质量结论。

## 4. 首次失败与复验

所有失败原件和重跑摘要均保留，没有删除失败记录后只报告最后成功。

- security 首次暴露零引用授权；migration-205 暴露无引用误 queued；maintenance-batch 暴露批次饥饿；baseline 暴露空白摘要。这些缺陷分别修复后复验。
- incomplete manifest 夹具最初直接违反 PG CHECK，已改为验证 23514 拒绝后再走合法发布；这是夹具问题。
- 真实链首次 search 504 和 MQ 清理超时，第二次顶层通过；证据不足以认定唯一根因。
- 初版故障 runner 的 docker stop 超过 8 秒；改为明确 SIGKILL 和独立 watchdog 后，MQ/core/etcd 复验。控制 PG 恢复约 43.8 秒，MQ 约 24.0 秒，仅为本机小样本。
- ACK 夹具最初有拓扑初始化超时、Claim 前 generation 错配；修正测试后通过，生产 ACK/重试逻辑未改。完整回归另暴露取消后连接重建丢失会话级 schema；replay 改用 DSN 带 search_path 的独立池，避免污染父夹具连接。
- 广范围 race 为 97 pass/1 fail：长文组件直接 Claim 后未启动生产续租，summary 约 32.97 秒，超过 30 秒 lease，随后 Fail 被 fencing 拒绝。没有 DATA RACE 报告；定向 race 13 pass/0 fail、7 顶层，不等于整个 aisystem race 全通过。
- 前端 lint 首次 4 个违规；改测试交互时两次未正确展开目录，最终通过单文件和全量。原件保留。
- 最终部署故障 runner 首次用错登录字段，收到 422 后没有执行后续业务断言；角色恢复后改为接口的 `login` 字段，重新演练，未把该夹具错误记成产品登录缺陷。
- API 派生镜像首次 COPY 到 /app/api，实际 entrypoint 是 /app/service；修正并核对运行二进制 SHA 后才验 HTTP。无效镜像登记为未使用产物。

## 5. 运行状态与保留资源

仅更新 E2 的 core-internal、ai-summary-worker、ai-committer、api 四个镜像；均用 Go 1.25.13 构建，镜像 ID、运行中 /app/service SHA 和健康状态一致，见 `manifest/deployed-binary-verification.json`。其他角色继续原测试镜像，并非全部部署到了最终源码。迁移修复在本地工具/隔离 schema 验证，未对 E2 常态库执行 prepare/archive。

mTLS 使用独立临时 core 容器，结束后回收自己的容器；常态 E2 未整体切换 mTLS。故障角色已恢复，卷全部保留。`bx-fullchain-20261106` 未操作；初始运行的 24 个容器镜像未变，原已停止的 6 个容器不在初始运行清单内，未把它们误记为本轮变更。

合成账号/上传、测试数据卷、镜像、原始失败日志保留。成功 schema/vhost/mTLS 夹具仅回收自己创建的资源。首次 ACK 拓扑初始化失败留下 70 个本轮队列，最终 messages=0、consumers=0，清单见 `manifest/ack-queue-residuals.json`；未 purge 历史 MQ，也未删除用户数据。

有界并发为每档 24 请求、1/4/8 并发，p50 57/73/223 ms，p95 288/253/307 ms。验证限定 scope 和 evidence token；未完成连接/限流/固定延迟 30 秒矩阵，H05 仍按未完整执行登记，不作为容量结果。

## 6. 场景登记

证据索引为最终 `manifest/cases.json`，各路径相对于证据根目录。整组标准与子集结果分别列出。

| ID | 优先级 | 状态 | 已验证范围与缺口 |
|---|---|---|---|

| A01 | P0 | PASS | 角色凭据键、实际三库身份和 TCP 目的地快照符合隔离；快照不代表持续网络审计。 |
| A02 | P0 | PASS | 关闭只读会话参数后，query 对 staging/基表/写入/DDL 仍被数据库授权拒绝。 |
| A03 | P0 | PASS | compute INSERT staging 与非法 SELECT/UPDATE/DELETE/active/receipt/DDL 负向检查；正常控制接纳发布另由组件链路验证。 |
| A04 | P0 | PASS | 业务只读真实拒写；显式空 AI DSN 不 fallback，实际业务/控制库误用被拒。 |
| A05 | P0 | PASS | 缺失、错误、重复 bearer 和 health 路径伪装拒绝；有效服务身份仍需有效业务 grant。 |
| A06 | P0 | PASS | 签名/密钥/audience/过期/等于到期/畸形/超长及对象/世代/输入/action claim 负向检查通过。 |
| A07 | P0 | PASS | 真实 MinIO：零引用拒发新 URL，旧世代拒绝；已发 URL 在 TTL 内仍可读且字节一致，到期后 HTTP 403；响应授权截止不晚于 processing grant。 |
| A08 | P1 | PASS | 实际镜像的临时 core 容器完成 mTLS RPC；错误 CA/SAN、过期/缺客户端、明文及错证钥拒绝。常态隔离栈仍使用原 TLS 配置。 |
| A09 | P1 | NOT_RUN | 配置层独立依赖和模型凭据已测；尚缺逐角色删除无关依赖的冷启动矩阵。 |
| B01 | P0 | NOT_RUN | 旧租约拒绝、staging 不可见、superseded 基线已测；未串起旧 worker 残留清理与新 worker 唯一成功的完整一次交错。 |
| B02 | P0 | PASS | 生产 RunWorker 在 SaveResult 后的实际 Basic.Ack 帧被拦住并 SIGKILL；重投无 compute 再调用、不增加 attempts，结果重复提交仍唯一。 |
| B03 | P0 | NOT_RUN | 12 类 manifest 篡改、完整性 CHECK、伪造 child range 和回滚已测；未验证所有拒绝进入持久诊断终态的完整链。 |
| B04 | P0 | PASS | 真实子进程在 AI 事务 pre-receipt advisory barrier SIGKILL，零部分发布；另有 receipt 写失败触发器回滚检查。 |
| B05 | P0 | PASS | AI commit/pre-Report 与 Report/pre-ACK 两个真实 SIGKILL 窗口；receipt 重用、后继唯一、实际结果消息重投已测。 |
| B06 | P0 | NOT_RUN | 两个独立 PG session 同 result 竞争仅一个 receipt/source；缺不同 result 同对象与 core committer 同时提交矩阵。 |
| B07 | P0 | NOT_RUN | 接纳冻结、重复 Report、后继唯一已测；缺控制确认/ensure 事务中断与配置漂移屏障。 |
| B08 | P0 | NOT_RUN | 旧 desired seq superseded 基线已测；缺 A/B source、旧 text/summary/vector 多阶段交错。 |
| B09 | P0 | NOT_RUN | summary-only 不调用 embedding、失败独立及旧失败不覆盖新成功已测；缺多目标完整交错。 |
| B10 | P0 | NOT_RUN | 同世代墓碑不复活基线已测；缺删除/投影晚到后同字节新世代的完整链。 |
| B11 | P0 | NOT_RUN | 未构造同字节跨 owner 多 entry、删除/改名/移动与重建并发夹具。 |
| B12 | P1 | NOT_RUN | 未构造模型响应丢失、staging INSERT 断连和接纳响应丢失的精确不确定结果矩阵。 |
| B13 | P1 | NOT_RUN | 已有 redo 留痕、续签与旧预算保留测试重跑；并发 redo 和所有 attempts/dispatch/recovery/deadline 邻值未穷举。 |
| C01 | P0 | PASS | 实际 E2 分别停止 query、AI PG、全部六个 AI 计算/提交角色；停机期间登录、readiness、目录、name 和原字节下载一致，query/AI PG 内容有限 503，恢复后可用。 |
| C02 | P0 | PASS | 实际 E2 分别 SIGKILL 控制 PG/MQ；停机期间 upload Create、Complete 和 reprocess 均受理，恢复后会话完成、job succeeded；核心登录/目录/name/字节下载持续通过。 |
| C03 | P0 | NOT_RUN | 专用 E2 broker 确实 SIGKILL/恢复并完成 durable work；confirm 前/后和未 ACK 三窗口未逐项注入。 |
| C04 | P1 | NOT_RUN | core-internal 中断时内容有限 503，恢复后可用；缺慢响应、scope 分页和 processing 获取中断矩阵。 |
| C05 | P1 | NOT_RUN | 缺 parser 进程 SIGKILL 和 MinIO 403/404/短读/错字节/超时完整故障夹具。 |
| C06 | P1 | NOT_RUN | 坏维度/数量/NaN/provider error/空白摘要均不得形成成功 manifest；缺真实模型 HTTP 429/500/超时/断流分类和恢复矩阵。 |
| C07 | P1 | NOT_RUN | etcd 中断/恢复已测；缺旧注册地址及双 query/parser 实例切换。 |
| C08 | P1 | NOT_RUN | 现有 core 分片接管由回归覆盖；未做 AI 双 committer 32 分片 SIGSTOP/失租/旧实例恢复专项。 |
| C09 | P1 | NOT_RUN | 缺角色小连接池耗尽、锁等待、短断连与恢复资源回落专用观测。 |
| C10 | P1 | NOT_RUN | 缺两个 owner 慢模型/parser 背景下核心推进、公平与背压专项。 |
| D01 | P0 | PASS | 真实 Nginx 上传后实际 ES/QD 命中及作用域 Ask/版本引用通过，三库真实链路另重跑。 |
| D02 | P0 | NOT_RUN | 真实后端故障回退与 ES 畸形 200 已覆盖；未逐一完成 ES/QD 超时/畸形/同时失效的对称矩阵。 |
| D03 | P0 | NOT_RUN | 无证明不调用外部、完整零命中有效、覆盖少一版本拒外部已测；缺错误 namespace/epoch 与旧 target 组合。 |
| D04 | P0 | NOT_RUN | 缺跨 owner/墓碑/旧世代/错 content-set/未知重复 child 的外部脏候选注入矩阵。 |
| D05 | P0 | PASS | AI PG 中断内容拒绝裸读；模型 revision 不兼容时 semantic 不可用，hybrid 文本降级。 |
| D06 | P1 | NOT_RUN | 未构造 199/200/201 跨批版本召回、排名合并去重及尾批资格。 |
| D07 | P1 | NOT_RUN | embedding revision 不兼容基线已测；缺 summary prompt/revision 与单类 projector epoch 切换矩阵。 |
| D08 | P1 | NOT_RUN | 全文不调用 embedding、name 在 AI 故障时可用；缺两类 projector 互不调用另一后端的完整计数。 |
| D09 | P0 | PASS | selected 0/1/49/50/51、重复与非法 ID、跨 owner 拒绝；all/folder 基线在真实链路与 gateway 回归覆盖。 |
| D10 | P0 | NOT_RUN | reranker 前、answer 前和 answer 中失效：前者零 Answer 调用、后者无返回；缺所有 scope/候选屏障及实际 move/ACL 更新。 |
| D11 | P0 | NOT_RUN | processing 旧世代拒绝、scope 回验基线已测；缺证据选取后对象变代及跨 owner entry 引用专项。 |
| D12 | P1 | NOT_RUN | 1001 稀疏资格、主体/过滤 cursor 拒复用已测；缺 L=10000 邻值与 2001 资格、不推进 cursor 矩阵。 |
| D13 | P1 | NOT_RUN | 非法 limit/sort/cursor/folder 参数已测；缺同名静态无漏重、转义字符和 rename/move/delete 翻页竞态。 |
| D14 | P0 | PASS | 真实引用包含 source/content/parent/child 版本、excerpt 和原文件下载；发布拒绝伪造 child 范围。 |
| D15 | P1 | NOT_RUN | 单文件超过两片及长文末尾证据已测；缺 50 文件软配额和 8000/12000 token 邻值矩阵。 |
| D16 | P1 | NOT_RUN | 未知/重复未知 citation ID 不生成引用，包含恶意指令的合成文本仍限 scope；缺空证据/空引用与真实模型输出质量评估。 |
| E01 | P1 | NOT_RUN | 本轮已重跑 parser 金标单元用例；未新增全套 D2/D4 格式/编码/sheet/损坏样本 manifest。 |
| E02 | P1 | NOT_RUN | 图片预览及未知二进制下载 SHA 已测；缺 MIME/扩展名冲突、加密/损坏/超限/扫描件状态完整矩阵。 |
| E03 | P0 | PASS | 固定 5 seeds×100 Unicode 文档性质检查；ID/rune 范围/预算/全覆盖/确定性及伪造范围发布拒绝。 |
| E04 | P1 | NOT_RUN | 长文尾部和规范 source blocks 全文摘要已测；未测 summary calls 预算 −1/等于/+1。 |
| E05 | P0 | NOT_RUN | 文本 ready 时 vector/summary 失败仍可 Ask，旧失败不覆盖成功；缺每个能力 pending/新失败/旧成功全部状态笛卡尔矩阵。 |
| E06 | P1 | NOT_RUN | 16 个开关组合的配置与模型依赖校验通过；未对全部路由及代表组合重新部署跑链。 |
| E07 | P1 | NOT_RUN | 缺 parser/summary 并发取消、模型超预算返回和 HTTP 客户端断连的精确观测。 |
| F01 | P0 | PASS | 六端点匿名拒绝、跨 owner、坏参数、65535/65536/65537 字节及超长问题共 24 检查通过；越权重建 404 已与文档对齐。 |
| F02 | P0 | NOT_RUN | 1/4/8 并发同 key 返回同 job、改 targets 409、job 完成；缺 intent/task/模型调用次数全链 SQL 对账。 |
| F03 | P0 | NOT_RUN | 缺用户多 targets 混合失败/耗尽/superseded 及 receipt 后断 Report 的 job 收尾专项。 |
| F04 | P1 | NOT_RUN | 真实文件 Ask、图片预览、不支持文件信息与下载已测；缺 summary pending/failed 时菜单入口完整浏览器覆盖。 |
| F05 | P1 | NOT_RUN | 现有 React 选择器回归已通过；真实浏览器跨页草稿/取消/49→50→51/空页扫描尚未逐项演练。 |
| F06 | P0 | NOT_RUN | 选中后删除返回 scope_invalid 并保留问题与选择；缺修正后重新提问及请求返回逆序竞态。 |
| F07 | P1 | NOT_RUN | 引用弹窗与原文下载、page_errors 空已测；缺 HTML/脚本样本、窄屏键盘和焦点返回专项。 |
| F08 | P1 | NOT_RUN | 缺浏览器 503/504/限流/取消/过期登录/刷新返回的完整 UX 故障矩阵。 |
| F09 | P1 | PASS | 实际 Web/Nginx 上传文本/PNG/未知类型，同源 storage、图片预览与下载 SHA 通过；13 检查，page_errors=[]。 |
| G01 | P0 | NOT_RUN | 真实三实例迁移 36/4/7 clean、误库拒绝及 archive 基线；缺 dirty/缺角色/缺迁移完整启动负向矩阵。 |
| G02 | P0 | NOT_RUN | 无停止标记拒绝、旧任务持久交接与预算基线已测；未构造仍活跃消费者/写者的实际检查矩阵。 |
| G03 | P0 | NOT_RUN | 205 对象跨 100/200/205；plan/handoff/enqueue 的实际子进程提交后 SIGKILL、同 run 重入账目通过；缺 99/101 邻值、批事务中断及不同 run 冲突。 |
| G04 | P0 | NOT_RUN | 205 对象五类各 41，旧预算/summary 保留与不确定结果隔离基线；未逐项新增所有 deadline/失败/结果不确定组合。 |
| G05 | P0 | NOT_RUN | 新独占 vhost 内 5 类真实旧消息持久隔离再 ACK，写失败不 ACK、重入无重复，core sentinel 保留；缺 durable COMMIT/pre-ACK 杀迁移工具。 |
| G06 | P0 | NOT_RUN | 可映射 observer 不改 frozen input、无法映射 job 得 durable conclusion 基线已测；未做 observer 附加与完成的真实并发屏障。 |
| G07 | P0 | NOT_RUN | 尚未发布的 queued 项会阻断 cutover-check；缺 quarantine/旧消息/legacy job 四类逐项阻断和人工审阅留痕。 |
| G08 | P0 | PASS | 隔离 schema 演练 archive up/down，旧表/summary/FK 恢复，保留历史数据；未在 E2 常态库执行 archive。 |
| G09 | P1 | NOT_RUN | 未演练新旧 query 混合数据兼容回退，不将 archive down 解释为服务无损回滚。 |
| G10 | P1 | NOT_RUN | 原有永久暂停旧队列不阻塞 dispatch 用例已由全量回归执行；缺 one-shot 失败及错误 Compose 所有权逐项部署注入。 |
| H01 | P0 | NOT_RUN | active、pending accepted 结果保护与 8 天策略基线已测；缺前/等于/后精确时间边界和 GC/重建并发。 |
| H02 | P0 | NOT_RUN | 未接纳物理投影的 orphan 持久事实和 404 幂等删除已测；缺删除超时/成功丢响应及跨世代旧 delete 矩阵。 |
| H03 | P0 | NOT_RUN | 缺控制证明拒清理的组件路径已测；缺证明后新 result/intent 并发及清理进程中断。 |
| H04 | P1 | PASS | 100 个受保护旧 artifact 后的第 101 个可清理项得到推进，受保护 100 个保留；GC receipt 游标修复已部署隔离 ai-committer。 |
| H05 | P1 | NOT_RUN | 1/4/8 每档 24 个混合读通过并记录 p50/p95；未做角色连接/限流/deadline 与 30 秒固定延迟矩阵，不作容量结论。 |
| H06 | P0 | NOT_RUN | 尚未实现多 owner 同字节、删除/改名/重建的有界状态模型交错。 |
| H07 | P1 | NOT_RUN | 尚未暂停单种计算角色积累受控 backlog 并核对其他队列公平、恢复排空和预算。 |
| J01 | P2 | BLOCKED | 未提供真实付费模型调用预算及可用专项凭据；本轮只用 fake provider。 |
| J02 | P2 | BLOCKED | 当前仅一台宿主机，没有第二应用主机/专用故障网络。 |
| J03 | P2 | NOT_RUN | 未建立三种不一致恢复点的三库+对象隔离恢复夹具。 |

## 7. Skip、排除项与后续门槛

- `internal/taskqueue/TestRabbitMQStuckOwnedConsumerHelper`
- `internal/taskqueue/TestStuckHandlerProcessHelper`
- `internal/taskqueue/TestStuckStoreHelper`
- `internal/taskqueue/TestRabbitMQTransport/broker_restart_preserves_confirmed_tasks`
- `internal/taskqueue/TestRabbitMQStability`
- `internal/taskqueue/TestRabbitMQCrashConsumerHelper`

逐项理由见 `manifest/skips.json`。四个历史 helper 的父用例已执行；新增 helper 在无子进程输入时正常 return，不新增 skip。专用 E2 broker 已重启并恢复，但不能把旧 restart 用例 skip 改记 PASS。长稳/浸泡明确 EXCLUDED。

优先补 P0：脏投影/namespace/epoch、多版本及跨 owner 同字节交错、多 targets job 收尾、控制确认事务中断、迁移 COMMIT/pre-ACK、GC 精确宽限及证明后并发；然后完成 P1 双副本/SIGSTOP、连接池/parser/模型故障、浏览器竞态与预算邻值。J01 缺付费预算、J02 缺第二主机均 BLOCKED；J03 是恢复夹具未建，保留 NOT_RUN。未执行内容不会用 health、队列归零或 archive down 代替验收。
