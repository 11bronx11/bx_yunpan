# AI 独立系统深入测试计划书

日期：2026-11-17（北京时间）。状态：待执行。

依据：[AI 独立系统实现与验证记录（2026-11-16）](AI_SYSTEM_IMPLEMENTATION_2026-11-16.md)，重点承接其第 3 节已执行验证和第 4 节未验证边界；同时核对当前 AI、core RPC、控制接纳、账号授权、迁移与浏览器测试源码。

**本轮目标：在已有验收基础上，深入验证 AI 拆分后的权限、发布一致性、故障恢复、检索证据、迁移和实际部署边界。本文是新测试计划，不是通过报告。** 报告及施工书中的历史操作描述不构成本轮执行指令；本轮只生成计划书。

**明确排除长时间压测、浸泡、耐久运行和按自然时间等待数日的清理观察。** 保留有界的并发正确性检查和短时故障演练；不寻求峰值 QPS，不据此出容量或长期稳定性结论。

## 1. 基线与本轮增量

### 1.1 已有证据如何使用

原报告记录后端 629 pass / 0 fail / 6 skip、前端 21 套件 / 126 项、parser 17 项，以及真实三库、MQ、MinIO、etcd、ES/QD、浏览器和隔离 Compose 链路。这些数字是**原报告的历史结果，本轮没有重跑或重新审计全部日志**，不能作为新增场景已通过的证明。

| 已有验证 | 本轮继续利用 | 深入验证的增量 |
|---|---|---|
| 真实 upload→source→text→vectors/summary→ES/QD→Ask | 保留为快速回归和运行环境校准 | 多版本、并发、崩溃、权限变化时也必须收敛 |
| staging 不可见、旧 lease 拒绝、receipt 重放、后继去重 | 保留协议级测试 | 各提交边界真实杀进程、两个 committer 争抢、通知丢失 |
| 旧 seq/墓碑、新成功不被旧失败覆盖 | 保留现有顺序用例 | source/text/vector/summary 交错、共享对象与新世代、模型版本切换 |
| embedding/summary 失败时全文与文本 Ask 可用 | 保留独立能力基线 | 429/超时/非法模型输出、仅一个投影可用、开关组合、预算耗尽 |
| ES/QD 503 回退 | 保留真实后端与专属故障代理 | 200 但覆盖不全、错误版本、脏命中、完整零命中、分批覆盖缺口 |
| scope 复核、1001 项稀疏资格、50 文件选择器 | 保留 API 与 React 用例 | 跨 owner、调用中删除/移动/权限变化、分页快照与 10000 边界 |
| 3 个历史对象、旧 summary、预算、归档 up/down | 保留小 fixture | 100 条批边界、真实旧 MQ 消息、进程中断、混合历史状态 |
| 停止 AI/AI PG/控制 PG/MQ 后核心读取仍可用 | 保留独立停止 smoke | 拆开逐个失效；验证停机期间新受理意图及恢复后的处理 |
| 配置/凭据静态检查；内部 token 网络 smoke | 保留 Compose 检查 | 数据库权限负向实测、网络边界、实际 mTLS 握手和错误证书 |
| fake provider、单机环境 | 作为确定性验收主环境 | 真实模型效果、跨主机仅在专项环境具备时单独验收 |

六个历史 skip 逐项登记：四个 helper 属于子进程入口，核实对应父测试是否执行；broker restart 改为本轮专用 broker 的短时恢复场景；长稳测试明确 EXCLUDED，不再作为待办。最终统计区分顶层、子测试、无测试文件和 skip，不机械要求仍为 629 项。

### 1.2 优先级与交付边界

- **P0：** 数据、权限、任务权威、发布和迁移安全。未通过不得给出本轮正确性验收通过。
- **P1：** 故障恢复、能力独立、文档兼容、浏览器、实际 mTLS、短时资源边界。纳入本轮完整验收。
- **P2：** 有条件专项：真实付费模型效果、跨主机、独立备份恢复。单独报状态；缺少环境不阻塞明确限定为单机/fake provider 的 P0/P1 结论。

全部新增场景初始为 `NOT_RUN`。下表中的“预期”是待检验要求，源码不满足时记缺陷，不能为了通过而改写预期。测试夹具不足记待补充；依赖缺失记 `BLOCKED`，不能变成 PASS。

## 2. 被测链路与环境

### 2.1 固定测试对象

```text
浏览器/Web → API → 业务 PG：文件/权限/对象/AI intent/用户 job/outbox
                    ↓
async-control → 控制 PG：接管/冻结任务/预算/结果接纳/冻结后继
                    ↓ MQ 工作通知
content / embedding / summary / text-projector / vector-projector
                    ↓ AI PG 不可变 staging manifest
控制结果接纳 → AI 独立结果分片 → ai-committer
                    ↓ AI PG：内容版本/active pointer/receipt/投影证明
控制确认 + 后继释放 → core committer → 业务用户 job 收尾

API → ai-query → core RPC scope/对象/证据复核
              → AI 发布视图 → ES/QD 或 PG FTS/pgvector → 按需模型
content → core processing grant → MinIO 原字节 → parser
```

业务 PG、控制 PG、AI PG 分别是业务事实、执行权威、派生发布状态的权威。不存在跨库原子事务；AI 已发布、控制未确认是可恢复中间态，不能直接判为不一致。

### 2.2 环境分层

| 层级 | 配置 | 用途与结论限制 |
|---|---|---|
| E0 | 确定性 fake provider、可控返回/屏障、纯函数或状态模型 | 契约、性质、精确交错；不证明网络/进程部署 |
| E1 | 专用迁移后业务/控制/AI 库、MQ、MinIO、Redis、etcd、真实 parser、真实 ES/QD | 真实协议和投影；共享 PG 实例只证明逻辑隔离 |
| E2 | 独立 Compose 项目，三 PG 独立容器/卷，真实 Web/Nginx，角色为独立进程 | 进程崩溃、权限、镜像、浏览器、单机多副本；不证明 HA |
| E3 | 可选，两台以上应用主机和专用故障网络/恢复副本 | 跨主机或恢复专项；基础设施未组集群时不宣称其 HA |

E2 常态每角色一副本。仅竞争/分片场景临时启两个 ai-committer、两个同类计算或 query 副本，完成后恢复登记的测试拓扑；不要同时把所有角色扩到两副本挤占主机。

### 2.3 执行前必须确认

1. 保存 HEAD、工作区差异摘要、被测源码/迁移文件哈希、镜像 digest、Go/Node/Python/PG/扩展/ES/QD/MQ/Compose 版本和脱敏配置。当前大量实现位于工作区，HEAD 不能单独标识被测版本。
2. 按实际连接核对三库身份、实例、端口和角色权限；当前基线为业务 36 / 控制 4 / AI 7、`dirty=false`，archive 1 为独立显式演练。执行时如版本变化，先更新矩阵。
3. 每次生成 `run_id`，登记数据库/schema、vhost、bucket、etcd prefix、ES namespace/索引、QD collection、容器/网络、故障代理和端口的所有权。
4. `TEST_*` 只能指向专用测试资源。部分兼容用例访问 public，outbox 夹具还克隆 public 表，不能把全量测试指向开发/业务库。PG/MQ 进程故障必须使用独立实例，schema/vhost 不能隔离停进程影响。
5. 配置从现有部署约定派生，在独立项目使用受保护的测试配置；不改用户 `deploy/.env`。原报告的测试项目/端口只是历史信息，先盘点再决定复用，不能直接认为仍空闲。
6. 测试 provider 记录调用类型、次数、请求摘要、任务与输入版本；只用合成文档。证明“没有调用模型”要有调用计数，而非仅看 HTTP 成功。
7. 串行冷构建；build、全量回归、浏览器与短时并发分开运行。为 DB 池、worker 并发、模型调用/字节预算、MQ 在途数保存启动值。
8. 首先重跑一条真实上传至 Ask 快速链路，确认权限、parser、中文检索和时间/证书有效期。失败先修环境，不带病开始随机矩阵。

不打印 DSN、服务 token、用户 JWT、签名 URL、私钥或模型 Key。trace/HAR/请求转储同样脱敏。失败保留现场，最终提交资源清单；不自动删除原有卷、数据、队列和历史产物。测试夹具只能回收本次明确拥有的临时资源。

### 2.4 时限与负载约束

| 项目 | 上限与做法 |
|---|---|
| 单个网络/停进程故障 | 通常 5–15 秒，最长 30 秒；代理或看门狗独立恢复 |
| 单个恢复观察 | 测试专用 lease/retry 配置下通常 120 秒内；最多 180 秒，超时即失败并取证 |
| 生产默认 lease/授权边界 | 单独做默认值静态校验和边界 fixture；不为了等默认 30 分钟任务超时而持续运行 |
| 并发正确性小实验 | 单档 30 秒、最多 60 秒；每种 1/4/8 并发，三个工作负载累计施压不超过 6 分钟 |
| 随机/模糊输入 | 固定 5 个 seed，每个 100–200 个有界操作；已有 fuzz target 每个最多 30 秒 |
| 重复故障 | 同一关键窗口至多 3 次有记录复现；首次失败保留，不循环到绿 |
| 生命周期 | 合成临界时间戳/可控时钟或测试数据回拨，检查 8 天窗口两侧；不改宿主机时间、不自然等待 |

整套分阶段执行可以跨多个工作时段；这不等于持续施压。任何阶段内存逼近执行前登记上限、连接超过配置额度、影响非测试服务或触发不变量破坏，立即停发新请求、恢复故障并取证。

## 3. 数据集与统一判定器

### 3.1 有界测试数据

| 数据组 | 最小组成 | 专门验证 |
|---|---|---|
| D1 身份/目录 | A/B 两个 owner；同内容不同 owner；A 下多层目录、重命名/移动；相同对象的多个 entry | 共享物理字节不共享权限；entry/object 身份区别 |
| D2 文本 | 中英混排、中文专有词、代码/JSON/YAML、空文档、Unicode/emoji、CRLF、支持编码与非法字节 | 解析、中文 FTS、位置、转义、空结果 |
| D3 长文 | 中/尾部独有标记、重复标题、跨片句子；父 2400/子 600 估算 token 的边界邻值；超过 summary 调用预算 | 不丢尾、不伪造偏移、完整性与显式 partial/失败 |
| D4 文档格式 | 文本 PDF/加密或损坏 PDF；DOCX 段落/表格；CSV 引号/多行；XLSX 多 sheet/空单元格；损坏 ZIP 容器 | 支持集合的真实提取质量和资源上限 |
| D5 负面格式 | PPTX、旧 DOC/PPT、扫描 PDF、图片、未知 MIME，以及扩展名/MIME/字节不一致 | 明确 unsupported/失败/能力状态；保留下载和图片缩略图 |
| D6 规模边界 | selected 0/1/49/50/51；范围上限 L−1/L/L+1（默认 L=10000）；召回批 199/200/201；资格扫描 999/1000/1001/2001 | 有界、完整分页、明确超限、跨批融合；大范围文件可用元数据 fixture，不上传万份字节 |
| D7 版本状态 | 未计算、staging、accepted 未发布、发布未回报、旧成功/新失败、墓碑、新世代、旧模型 revision | 发布闸门与版本依赖 |
| D8 迁移 | 205 个历史对象跨越 100 条批边界，混合成功/失败/耗尽/不支持/无引用/结果不确定；旧 job 和真实旧 MQ 消息 | 分批、幂等、账目与隔离结论；保持小字节量 |

格式样本、原字节 SHA、预期文字/单元格/页码和支持状态入 fixture manifest。D5 不把未实现格式升级为本轮支持承诺；压缩炸弹等使用受控小样本触发限制，禁止生成真实失控负载。

### 3.2 每组共用的不变量

| 编号 | 必须成立 |
|---|---|
| I01 权威/权限 | 未授权文件的正文、名称、excerpt、ID、版本引用不得通过搜索/Ask/列表/job/概览暴露；未授权证据不进入 reranker/answer |
| I02 发布 | query 只看发布视图；staging、未接纳、校验失败或不同 object/generation 的输出不能成为 active |
| I03 幂等 | 同一 result 的 receipt/发布效果唯一；消息、回报与请求重复不会重复切 active、重复后继或重置预算 |
| I04 版本 | source/text/vector/summary 的 seq 单调；active 依赖正确；旧失败不覆盖新成功，旧 embedding 不匹配新 text，墓碑同世代不可复活 |
| I05 执行 | 控制库是唯一执行权威；过期 token/代次不能提交；computed 重投同一结果，不重新调用 parser/embedding/summary |
| I06 后继/job | 接纳时冻结后继，控制确认与释放原子；superseded 不释放错误后继；job 有可解释持久结论，AI 不写业务 job |
| I07 投影 | 外部命中必须同时满足 AI active/coverage/namespace/epoch 证明；不完整覆盖整体分支回退，AI PG 不可用不能绕过证明 |
| I08 能力 | 模型、summary、vector、text/两类投影故障互不扩大；fulltext 无 embedding/reranker/chat 调用；name 无 AI 依赖 |
| I09 清理 | active、未提交结果、receipt、墓碑、必要投影删除事实保留；过期清理不能跨世代或删除当前输出 |
| I10 保留迁移 | 旧数据/预算/deadline/停止结论/原消息持久去向可对账；隔离结果不丢弃、不偷跑模型重算、不 purge |
| I11 核心 | AI 测试前后配额、引用、上传、分享、缩略图/下载一致；AI 故障不使 API 核心 readiness 依赖控制库/MQ/AI PG |
| I12 有界 | 连接、在途、重试、模型调用、输出字节、证据 token、scope 枚举与维护 batch 均受实际配置约束 |

同库不变量用同一只读 SQL 快照核对。跨库对账先停新输入、排空允许的在途，再按 task/result/receipt/版本逐项比较；运行中的合法窗口须结合屏障与日志判断。队列深度归零、readiness 成功、模型返回一段文本均不能代替这些判定。

每例证据连通 `run_id → request/job/intent → object/generation → task/token/result → artifact/hash → receipt/active → 控制确认/后继 → Search/Ask/citation`。记录注入前、窗口中、恢复后的快照及调用计数。

## 4. 场景 A：角色隔离、鉴权与实际 mTLS

| ID/优先级 | 操作与注入 | 验收/证据 |
|---|---|---|
| A01/P0 | 对 API、query、compute、AI committer、core-internal 的实际容器环境、连接和网络目的地盘点 | API 无 AI/控制/MQ 凭据与连接；AI 在线角色无业务 SQL；query 无控制/MQ；parser 无 PG/MQ。配置文本和实际连接分别取证 |
| A02/P0 | 用 query 账号 SELECT staging/基表、写发布表/视图、建表；关闭只读会话参数再尝试 | 数据库授权仍拒绝越权；可 SELECT 允许的发布视图，不能仅凭 default_transaction_read_only 判隔离成立 |
| A03/P0 | 用 compute 账号插 staging 后尝试 SELECT 私有 staging、UPDATE/DELETE manifest、改 active/receipt；committer 正常发布 | 只允许授权的 INSERT 和发布视图/投影目录读取；非法 DML/DDL 被拒绝。测试发布使用正常控制接纳，不能裸 SQL 代替 |
| A04/P0 | core 只读账号写业务表/DDL；AI 迁移 DSN 缺失；误把业务/控制库配为 AI 库 | 真实权限拒写；缺少显式 AI DSN 不回退业务库；核验实例/数据库身份，排除仅比较字符串的假隔离 |
| A05/P0 | 服务 bearer 缺失/错误/重复 header/错方法前缀；有效 token 配伪造 scope/processing grant | 服务身份不能替代用户/任务授权；health 按当前公开约定单独验证，不能据 health 绕过业务 RPC |
| A06/P0 | grant 改 principal/object/generation/action、错 audience/公钥、过期、畸形/超长输入；scope grant 调 processing 及反向使用 | 全部拒绝；边界到期语义一致；无正文/对象读取授权、无模型调用、无业务泄漏 |
| A07/P0 | processing grant 对象失引用/世代改变；记录当前获取对象 URL 的调用，等待短期授权边界 | 新授权拒绝失效对象；既发 URL 仅在其 TTL 内按契约工作，不宣称立即撤销；日志不泄漏 URL |
| A08/P1 | 实际部署内部 mTLS：合法 CA/客户端/服务端证书；随后错 CA、错 SAN、过期、无客户端证书、证钥不匹配、明文连 TLS 端口 | 合法链路贯通；负向握手或启动失败；不静默降级明文。证书有效期用专属证书生成，不改机器时间 |
| A09/P1 | query/worker 冷启动时分别移除无关依赖、模型 Key；关闭向量/summary/Ask 后再启动 | 只要求该角色/能力实际必需依赖；文本准备无需模型。启动错误与 readiness 可解释，不能把无关依赖变成强制依赖 |

源码落点：`platform/serviceauth/{auth,transport}.go`、`core/service.go`、`platform/dependencies/ai.go`、`platform/config/ai_split.go`、`deploy/ai-role-init.sql`。这些包缺少专门负向测试的部分须新增；静态查看 GRANT 不替代真实账号执行。

## 5. 场景 B：发布协议、崩溃窗口与版本竞争

精确窗口优先用测试专属屏障/代理；不能靠 sleep 猜时序。屏障不接入线上调度，不增加第二套恢复权威。进程验证至少覆盖 B02/B04/B05 的一次真实 SIGKILL；其余可先通过组件屏障穷举。

| ID/优先级 | 操作与注入 | 验收/证据 |
|---|---|---|
| B01/P0 | staging manifest 后控制接纳前失租；旧 worker 晚到；新 worker 正常完成 | 旧 token/代次接纳失败，staging 不可见；新结果唯一生效；未接纳残留有清理去向 |
| B02/P0 | 接纳结果持久后、工作 ACK 前杀 worker；重复工作通知与结果通知 | computed 仅补发原结果；parser/模型计数不增加；重算不能被 receipt 幂等掩盖 |
| B03/P0 | task/generation/input digest/artifact hash/kind/object/世代/output ref/行数/字节数逐字段篡改；manifest 不完整 | 每种不匹配拒绝发布；无 active/receipt 半写，错误有持久可诊断结论；正确结果仍可继续 |
| B04/P0 | AI 本地事务内杀 committer/断 AI PG；receipt 与 active 写入之间安排失败 | 事务整体回滚或整体提交；不存在有 active 无对应提交证明的部分状态 |
| B05/P0 | AI 事务提交后屏蔽控制 Report；重启 ai-committer，重复通知；再在 Report 后 ACK 前杀进程 | receipt 复用；active/内容行不重复；控制最终确认；后继只释放一次，core/AI 结果互不误消费 |
| B06/P0 | 两个 ai-committer 同时处理相同 result、不同结果的同对象；core committer 同时工作 | receipt 唯一、对象串行化正确；effect target 和结果分片隔离；无跨 lane 效果 |
| B07/P0 | 后继冻结后、控制确认/ensure 事务中断；重复 Report | 冻结输入不随配置漂移；确认与后继 ensure 同成同败，无丢后继/重置预算 |
| B08/P0 | source A→B；B 先发布，A 的 text/summary 晚到；新 text 后旧 vector 晚到 | 旧依赖结果 superseded；query 不组合不同 source/content/set；旧 vector 不覆盖当前 text |
| B09/P0 | vectors-only/summary-only/text-only 与完整重建交错；同 seq 成功后晚到失败；新失败保留旧成功版本 | 各 desired seq 独立，latest_build/freshness/available 分开表达；summary-only 不新增 embedding，失败不删除可用旧成功 |
| B10/P0 | 计算/投影期间删最后引用并 tombstone；旧输出晚到；随后同字节新上传产生新世代 | 同世代不复活；新世代不受旧 publish/delete 影响；不能用 object UUID 相同忽略 generation |
| B11/P0 | 同字节两个 owner/多个 entry，删其中一个、重命名/移动另一个，同时重建 | 不误给仍有引用对象写墓碑；内容可共享，文件身份、展示名与访问权限按当前 entry 复核 |
| B12/P1 | 模型调用已发生但响应丢失、staging INSERT 连接断开、接纳响应丢失 | 区分计算前失败与不确定结果；只对已持久 computed 保证不重算；未持久结果可能有限重试，调用计数和预算如实记录 |
| B13/P1 | attempts/dispatches/recoveries/deadline 边界；停止任务 redo 重复/并发/前提失效 | 有限停止；redo 留痕子 intent、续签 grant、重复去重且原预算不变；失引用/新 intent 拒绝 |

B03 使用隔离的防御性坏数据 fixture，不能把直接篡改表得到的拒绝当成普通用户 E2E。B12 不要求跨远端模型做到调用恰好一次，只要求持久计算结果不被重算和预算有界。

## 6. 场景 C：故障隔离、恢复与独立进程

| ID/优先级 | 短时故障与并行探测 | 验收/证据 |
|---|---|---|
| C01/P0 | 分别停止 query、全部 AI 计算/提交角色、AI PG；同时登录/目录/name/下载及内容请求 | 核心能力按依赖继续可用；AI 请求有限失败且错误明确；query/AI PG 停机不走外部索引裸读 |
| C02/P0 | 分别停止控制 PG、MQ；停机期间创建上传/完成命令和 reprocess，再恢复 | API 只提交业务意图且不等待控制/MQ；异步不要求故障期间完成；恢复后逐项处理，用户 job 最终可解释，不丢意图 |
| C03/P0 | 在 E2 专用 broker 做一次进程重启；分别落在 confirm 前/后及消费者未 ACK 窗口 | 已持久任务/结果可恢复；不确定发送允许重复但无丢业务效果；预算不无限增长。只重启专用 broker |
| C04/P1 | core-internal 中断/慢响应，scope 分页中断，processing 获取对象中断 | query/worker 有界失败或重试，禁止降级越过授权；失败后新请求可恢复，无模型泄漏 |
| C05/P1 | parser 子进程 SIGKILL、超时、畸形响应；MinIO GET 404/403/短读/超时/返回错误字节 | source 不伪成功；大小/hash/质量按契约验证；已有发布版本可读，下载/图片链不被 parser 失败阻塞 |
| C06/P1 | 各模型 429/500/超时/断流；embedding 维度错误/NaN/数量缺失；summary 空白或无效结构 | 分类明确、有限重试；坏向量/摘要不能发布为成功；无无界模型调用，text 和独立分支仍可用 |
| C07/P1 | etcd 不可用、旧地址注册、query/parser 实例退出后重注册；两 query 实例切换 | 按 resolver 契约恢复，无无限请求挂起；core 文件操作不依赖 AI 服务发现；副本不使用进程本地授权结论 |
| C08/P1 | SIGSTOP 一个持租 worker/committer 至测试 lease 到期，第二实例接管后恢复旧进程 | 旧实例被 fencing；新实例有效接管。两个 AI committer 的 32 个 AI 结果分片覆盖与 core 分片分别核对 |
| C09/P1 | 控制/AI PG 短连接中断、锁等待；小连接池耗尽一个角色；停止新请求后恢复 | 事务/请求按 deadline 退出；保留关键控制预算；连接/协程回落至可解释区间，无孤儿事务持续持锁 |
| C10/P1 | 模型/parser 慢任务期间继续核心上传/校验/缩略图；两个 owner 任务交错 | 计算队列/预算分离，核心可推进；公平与背压按配置取证，不据此推断生产吞吐 |

每个故障先做健康对照，单因素完成后再做四个高价值组合：`控制 Report 断开+重复通知`、`新 text 发布+旧 vector 晚到`、`删除+外部投影晚到`、`ES 故障+embedding 失败`。不做不可解释的全依赖同时断网随机风暴。

## 7. 场景 D：检索证明、scope 与 RAG 证据

### 7.1 全文、向量和投影

| ID/优先级 | 操作与注入 | 验收/证据 |
|---|---|---|
| D01/P0 | ES/QD 正常真实命中；同时记录 namespace/target/epoch、active version 与覆盖证明 | 证明实际调用真实后端，命中版本属于当前可见 scope；不能只看返回文本一样 |
| D02/P0 | ES 503/超时/畸形 200；QD 同组独立注入；再同时失效 | 分别 PG FTS/pgvector 回退；另一分支独立；错误/降级原因与实际 backend 一致 |
| D03/P0 | 有完成证明但实际返回零命中；无证明、覆盖缺一个版本、错误 epoch/namespace、旧 target 仍有命中 | 完整零命中是有效结果；覆盖缺失整分支回退，不把零命中自动当故障，也不漏掉未覆盖文件 |
| D04/P0 | 外部索引返回跨 owner、旧世代、墓碑、错 content/set、重复/未知 child | 过滤或拒绝非法候选，不能向用户/模型暴露；最终 core 回验仍执行；active/coverage 证明不能只在写侧存在 |
| D05/P0 | AI PG 断开但 ES/QD 正常；query embedding 不可用/模型 revision 不兼容而 pgvector 正常 | 前者拒绝绕过证明；后者 semantic 明确不可用，hybrid 可留文本分支且声明降级 |
| D06/P1 | 199/200/201 及更多版本跨批召回；不同批分数重复、相同 child 多次出现 | 先合并分支排名再融合，结果去重、顺序确定，末批资格不被前批截断 |
| D07/P1 | embedding revision 切换、summary prompt/revision 切换，单独更新 text/vector target epoch | 只失效需要重建的能力；旧向量不冒充兼容；更新一类投影不强迫另一类或模型重跑 |
| D08/P1 | 无模型凭据/禁向量、全文查询、name 查询、两类 projector 独立启动 | fulltext 不调用任何模型，name 无 AI RPC；text projector 无 QD 调用，vector projector 无 ES 调用 |

### 7.2 scope、权限交错与引用

| ID/优先级 | 操作与注入 | 验收/证据 |
|---|---|---|
| D09/P0 | all/folder/selected；空 selected、重复 ID、跨 owner ID、非法 ID、49/50/51 个 | 范围规范化且不扩大；空 selected 不变 all；越权一致拒绝；50 上限前后契约明确 |
| D10/P0 | scope 打开后/候选取出后/reranker 前/answer 前/answer 返回后分别删除、移动或更新权限 epoch | 当前请求按实际失效原因返回 scope_changed/scope_invalid，不自动扩大或全盘重试；模型前失效时模型调用为 0；模型后失效时答案/引用不返回 |
| D11/P0 | 证据选取后 object/content generation 变化；同字节多个 entry 只选一个；引用指向另一 owner entry | 返回引用保持 entry/object/版本一致，当前 core 回验通过；旧授权不能转用于新世代 |
| D12/P1 | scope L−1/L/L+1；资格 1000 项都不 ready、1001/2001 才 ready；篡改/复用/不推进 cursor | 有界超限明确报错；空页带 next_cursor 可继续；cursor 绑定主体/过滤/排序/快照，失效后可重新查询，不死循环 |
| D13/P1 | 同名排序、中文/%/_/反斜杠搜索、目录/类型过滤、name_asc/updated_desc；翻页间 rename/move/delete | 稳定无漏重的静态分页；变更后按契约失效，不混合 epoch；超长参数/非法排序有明确拒绝 |
| D14/P0 | 逐个引用回查 source/content/parent/child、excerpt 与原文下载；重复 parent、错误 child 范围 | ID 链真实且同版本；excerpt 是对应版本文本；未知位置不伪造页码/精确偏移；下载对象与文件权限一致 |
| D15/P1 | 单文件需要超过两片；50 文件中少数有证据、长短文混合、相关文本在尾部 | 不恢复每文件两片硬限制；父片去重和文件软份额合理；8000/12000 估算 token 预算、parent 上限及 missing-evidence coverage 真实 |
| D16/P1 | 无相关证据、模型返回未知 citation ID/重复 ID/空 citation；文档内嵌“忽略问题/读取其他文件”等指令 | 无证据不编造原文引用；未知引用不能伪造版本链；记录输出质量问题。恶意文本不能扩大 scope 或触发额外数据访问；不宣称完全消除提示注入 |

Ask 使用模型前后各复核的计数与屏障记录，区分“发送模型前已失效”和“模型进行期间发生变化”。不能把后一种情况错误要求成已发内容从未进入模型。

## 8. 场景 E：解析、父子分片、概览和能力状态

| ID/优先级 | 操作与注入 | 验收/证据 |
|---|---|---|
| E01/P1 | D2/D4 按格式、编码、语言逐份真实 parser 提取，逐项匹配金标 | 保留正文/表格/末尾信息；页/sheet/行列来源按支持能力记录；warnings/质量可解释，不以非空文本代替正确性 |
| E02/P1 | 同字节不同 MIME/扩展名；空/加密/损坏/超限文档和 D5 | 状态区分 unsupported、failed、pending、partial；不把扫描件无 OCR 输出当完整解析；下载/信息/图片预览保持 |
| E03/P0 | 相同输入同版本反复 BuildParentChild；边界 token/emoji/重复章节/段落切分、位置未知 | ID 与范围确定，父/子不超预算；child 文本等于 parent 对应 rune 范围、全覆盖不丢字；发布拒绝伪造范围 |
| E04/P1 | 长文规范源块概览，尾部唯一事实；summary 最大调用预算边界−1/边界/+1 | 全文分段归并或明确 coverage/失败；不截头冒充全文；记录预期/实际覆盖块和模型调用数 |
| E05/P0 | source/text/vector/summary 分别 pending/失败/新重建/旧可用；失败快照乱序发布 | supported/available/freshness/latest_build/askable 相互一致；summary pending/failed 不挡 text-ready 问答，旧失败不覆新成功 |
| E06/P1 | AI_ENABLED/ASK/VECTORS/SUMMARY 的 16 个布尔组合做配置/路由检查；代表组合跑真实链路 | AI 禁用明确 503、name 可用；其余能力开关独立；关闭向量的文本 Ask 仍需 answer 模型，不能宣称完全无模型 |
| E07/P1 | parser/summary 并发短取消，模型返回超过预算；HTTP 客户端断开 | 有界返回与取消传播，临时输出不误发布；调用/字节预算不因客户端重试无限增加 |

文本格式支持范围以当前实现为准。PPTX、旧 DOC/PPT、OCR 的“明确不支持/有限提取”是合法预期，不能为了本轮验收虚构全面支持。

## 9. 场景 F：HTTP 契约、用户作业与浏览器

| ID/优先级 | 操作与注入 | 验收/证据 |
|---|---|---|
| F01/P0 | 六类公共 AI 接口逐项对照 OpenAPI/前端；无认证、跨 owner、坏参数、64KiB 请求体边界、超长问题 | 状态/错误码/字段符合契约；无 500 堆栈/隐私泄漏；HTTP handler 手写实现与嵌入文档一致 |
| F02/P0 | 同 idempotency key 同参数重复/并发；同 key 改 targets；新的 key 叠加新重建 | 同命令返回同 job，参数冲突 409；合法新重建单调推进；验 intent/task/job/模型次数，不只验响应 |
| F03/P0 | 用户 job 多 targets 一成功一失败、superseded、耗尽、receipt 后断 Report；重启后查 job | 核心 committer 汇总持久结论，能力结果完整；AI 不写业务 job；无永久 processing、无假全成功 |
| F04/P1 | 文件菜单/概览/上传完成进入指定文件 Ask；不支持文件/图片仍可信息、下载、预览 | URL/应用 scope 与实际请求一致；summary pending/失败不影响已 ready 文本入口 |
| F05/P1 | 选择器跨页/搜索草稿、应用/取消/清空、49→50→51、空页继续扫描、关闭再开 | 保留/提交语义一致，51 阻止；取消不偷偷应用；清空不把非法 selected 自动转 all |
| F06/P0 | 选中后删除/权限失效，提问返回 409；修正选择再问；并发请求返回顺序颠倒 | 保留问题和问题文件提示；禁止静默扩大范围；旧响应不覆盖新问题/新 scope |
| F07/P1 | 引用弹窗/原文下载；文件名或 excerpt 含 HTML/脚本样本；超长名称/中文/窄屏/键盘操作 | 文字按内容安全呈现；可关闭/焦点可回到触发处；下载对应原文件，网络/页面无未处理异常 |
| F08/P1 | request 504/503/限流、浏览器刷新/返回、请求取消、过期登录 | 用户能分辨并重试；无重复自动提交、无限轮询；日志/截图/trace 脱敏 |
| F09/P1 | 真 Web 容器/Nginx 浏览器上传文档/图片/未知类型并下载 | `/api` 与同源 `/storage` 实际贯通；字节 SHA 一致。Go 测试反向代理浏览器场景与真实 Nginx E2E 分别标注 |

浏览器最低 Chromium 全场景；Firefox/WebKit 可列扩展兼容项，缺少引擎不能声称已通过。React 测试不替代真实浏览器，已有 13 项浏览器检查不替代新增异常/交错场景。

## 10. 场景 G：保留迁移、真实旧消息与归档回退

全部使用可丢弃的历史数据库副本/fixture。**不得对现有业务库直接运行 prepare/archive，不重跑核心 async-import。** 旧格式消息从对应旧契约构造或脱敏样本恢复，不拿新协议消息冒充旧消息。

| ID/优先级 | 操作与注入 | 验收/证据 |
|---|---|---|
| G01/P0 | 新安装三库迁移；已有旧库升级；缺迁移/dirty/错库/缺角色；归档 lane 独立运行 | 迁移版本可追溯，错误阻断启动/交接，不自动 fallback；业务 36 不自动移动旧表 |
| G02/P0 | 未设停止标志、仍有旧 control lease/消费者/写者时 prepare；记录与线上实际检查分别验证 | 不满足交接前提时拒绝；不能把仅设置环境变量等同旧写者真实停止 |
| G03/P0 | D8 的 plan/handoff/enqueue 分别在第 99/100/101 附近杀工具后用同 run_id 重启 | 游标/manifest/摘要持久；重入不漏不重、不重置预算、不重复 intent；新 run 与同 run 的冲突行为明确 |
| G04/P0 | 旧成功、停止/预算耗尽、过期、无引用、不支持、无法证明 computed 输出逐种交接 | 原 attempts/dispatches/recoveries/max/deadline/结论保留；不确定结果隔离并可检查，不丢结果或直接模型重算 |
| G05/P0 | 真实旧 MQ 未投/待 ACK/重投/未知格式/旧结果；持久转移后 ACK 前杀工具，再重启 | 每条有 durable 接管/隔离事实后才 ACK；允许重复但不漏；无 purge；未知消息留有定位和处理结论 |
| G06/P0 | 历史 processing job 无映射、有映射但子任务已冻结；附加 observer 与任务完成并发 | durable observer 不改 frozen input/预算；可映射 job 跟随后继收尾，无映射得 needs_attention，不能永久挂住 |
| G07/P0 | rebuild 未发布、quarantine 未处理、消息交接未完成、legacy job 未收尾时 verify/archive | verify 不冒充完成，cutover-check 阻断 archive；“审阅隔离项”如何留痕按实际契约验证 |
| G08/P0 | 完成重建后 archive up/down；检查 7 旧表、旧 summary 原文、外部 FK/约束 | 数据与 FK 定义/校验状态恢复；不反向伪造新 embedding/summary 的旧格式；不影响核心业务事实 |
| G09/P1 | archive 前后记录新旧文件/新增引用；停止新发布后演练旧 query 有限回退 | 明确旧 query 能覆盖哪些保留数据、哪些新文件不可覆盖；表归位不等于服务兼容回滚，不宣称无损双向切换 |
| G10/P1 | 新部署时 migration 快速成功/失败退出；Compose 所有权不符；旧队列永久暂停 | one-shot 退出码检查真实生效；不误操作他项目；legacy infinity 暂停不阻塞新上传/AI 队列派发 |

原迁移源码 fixture 明确没有旧 broker 消息，迁移消息账目不能用“有过真实 vhost smoke”替代。G05 是本轮新增必须补齐的实际交接验证。

迁移账目至少列出：`历史对象总数 = 重建/保留停止/不支持/无引用/隔离等互斥结论之和`；历史任务、job、消息分别有自己的计数和关联，不能把不同维度相加。原字节/旧 summary/FK 摘要和旧消息持久去向在迁移前后比较。

## 11. 场景 H：维护清理与有界并发正确性

### 11.1 清理不自然等待

| ID/优先级 | 操作与注入 | 验收/证据 |
|---|---|---|
| H01/P0 | 8 天宽限的前/等于/后边界，active/retired/staging/未提交结果并存；GC 同时重建 | 仅清理有控制证明且过宽限的可删项；active、pending receipt/result、墓碑保留；默认窗口配置单独核验 |
| H02/P0 | 外部字节写成功但结果未接纳；manifest 待清理；物理 delete 超时/404/成功丢响应 | 持久 orphan 删除事实不因 manifest 删除而丢失；404 按幂等完成；旧删除不碰新 epoch/generation |
| H03/P0 | 收集控制证明时控制 PG 不可用；证明之后新结果/新 intent 交错；GC 中断重启 | 缺证明拒绝冒险删除；并发安全前提实际成立；重启可继续，不引入独立清理租约/无限重试 |
| H04/P1 | 多于 100 个候选 artifact/版本，部分被 pending 保护；连续有限批处理 | batch 有界并可继续，不因前一批受保护项长期遮挡后续；活跃数据/receipt 不受影响 |

边界用测试数据库 timestamps 或可控 clock 构造。不得缩短实际部署的宽限/对象生命周期来凑通过，也不修改 MinIO 正式输出生命周期。

### 11.2 三个短时工作负载

| ID/优先级 | 工作负载，每档 1/4/8 并发 | 检查内容 |
|---|---|---|
| H05/P1 | Search/Ask/资格列表混合读，fake provider 固定延迟/响应 | scope/token/连接有界、deadline/限流正确；记录 p50/p95 与错误分类，仅作复现环境下的观察 |
| H06/P0 | 同一对象重建/重复命令/删除/rename，多 owner 相同字节 | 重点验 I01–I06、I11；屏障驱动关键交错，不靠大请求量碰运气 |
| H07/P1 | 暂停单种计算角色积累有界任务后恢复，另一队列继续工作 | 背压、公平、重试/调用/连接不失控；停输入后排空并核对 receipt/job；不寻求饱和容量 |

每档默认 30 秒且有总请求数上限，三个负载总施压不超过 6 分钟；恢复观察不继续生产新压力。输出中不报告“最大 QPS”“长期内存无泄漏”“生产容量达标”。

## 12. 有条件专项：真实模型、跨主机与恢复

| ID/优先级 | 范围与最小样本 | 单独验收方式 |
|---|---|---|
| J01/P2 | 真实 embedding/reranker/answer/summary；12 份合成小文档、20 个金标问题，含无答案/尾部/多文件/冲突事实/恶意文档指令 | 执行前固定模型/revision/参数、token/调用/费用硬上限；记录召回、引用支持度、无答案表现、尾部覆盖和人工逐题结论。无预算/凭据即 BLOCKED，不以 fake 回答质量代替 |
| J02/P2 | 两应用主机，AI 计算/提交与 query 跨主机；一个主机/单向网络故障≤30 秒 | 复用 B05/C08/D10；记录真实物理故障域。单节点 PG/MQ/MinIO 不据此成为 HA；没有集群就排除对应集群切换结论 |
| J03/P2 | 小测试集三库+对象清单备份到隔离副本；AI 发布领先控制确认、控制结果未发布、业务 intent 未接管三种恢复点 | 禁止新写者并行；核对 event/task/result/receipt、墓碑与 active 后再开放。只有被 retained history 支持才重放；无法安全恢复明确阻断/留 needs_attention，不能重置任务预算冒充恢复 |

J03 检查保留窗口与恢复点的兼容条件，不承诺任意时点三库独立回滚都能自动恢复。恢复实验每个小样本预算 10 分钟，超时保留现场；不开展大库备份吞吐或多日容灾耐久测试。

## 13. 测试资产与执行入口

### 13.1 可复用资产和新增位置

| 当前真实存在的测试/脚本 | 对应基线 | 需要补充 |
|---|---|---|
| `aisystem/integration_test.go`：`TestAcceptedPublishReceiptAndAtomicContinuations`、`TestSupersededStagingAndTombstoneNeverBecomeVisible` | B01/B05/B07/B08/B10 的部分组件路径 | 精确崩溃窗口、真实进程与双 committer、字段篡改矩阵 |
| `aisystem/capabilities_integration_test.go`：`TestIndependentFailuresFullTextAskAndSummaryCoverage` | D05/D08/E04/E05 | provider 负向响应、coverage/版本全矩阵、模型前后权限交错 |
| 同文件：`TestMaintenanceIdentityAndGraceProtectAcceptedArtifacts`、`TestRetiredUnacceptedProjectionIsRememberedUntilPhysicalDeletion` | H01/H02 | 时间邻值、并发证明、更多批次和真实删除响应不确定 |
| `aisystem/migration_integration_test.go` 三个现有测试 | G03/G04/G06/G08 的小 fixture | 205 对象、真实旧 MQ、SIGKILL、升级/回退独立进程 |
| `aigateway/integration_test.go`：`TestSparseEligibilityCursorAndCoreGrantBoundaries` | D09/D12、name 独立 | 主体/过滤/epoch/cursor 全矩阵，HTTP 契约与限流 |
| `bootstrap/ai_split_integration_test.go`：`TestAISplitRealDependencies` | 真实三库/MQ/parser/etcd/ES/QD、上传到 Ask | 仍为 Go 测试内装配角色；进程级 E2/mTLS 需要另补 runner |
| `asyncjobs/ai_admin_integration_test.go`：`TestAIRedoRenewsGrantAndRetainsStoppedBudget` | B13 | 并发 redo、坏前提、跨 job 结论与真实 CLI |
| `ai/rag/parent_child_test.go`：`TestParentChildCoverageAndStableRanges` | E03/E04 | Unicode/空白/边界性质与有界随机输入 |
| `platform/serviceauth/`、`core/`、`aiprotocol/` | 当前源码入口 | 补专门 auth/TLS/契约/处理授权负向与性质测试，当前不可视为已有完整覆盖 |
| `deploy/tests/ai-browser.cjs`、React 的 AIAsk/AskFilePicker 测试 | F04/F05/F09 部分 | 扩异常/竞态/焦点/错误反馈；真实 Nginx 与 fixture 反向代理分开运行 |

新增测试优先放已有包内，独立进程/故障代理放测试专用 runner。每个新增测试标注场景 ID；本计划不虚构已经存在的 test 名或命令开关。没有精确注入点先补最小夹具，不通过直接改生产表状态假装进程恢复。

### 13.2 现有命令模板（计划执行，不是本轮执行记录）

以下后端命令在 `backend/` 下运行；先完成 E1 专用环境预检并通过受保护配置注入 `TEST_*`，不要把凭据写进命令行或报告。测试子 shell 只继承本次登记的开关，清除历史长稳/旧容器重启开关，避免全量回归意外执行本轮排除项目；C03 的新控制链路 broker 恢复由独立 runner 明确安排。

```bash
# 仅在本次测试子 shell 中清除；不修改配置文件
unset TEST_STABILITY_DURATION TEST_RABBITMQ_CONTAINER

go build -p=1 ./cmd/...
go vet -p=1 ./...

# 当前 AI 协议/能力/维护/迁移与 gateway/admin 基线
go test -json -p=1 ./internal/aisystem ./internal/aigateway \
  ./internal/asyncjobs ./internal/ai/rag -count=1 -timeout 8m

# 真实三库 + MQ/MinIO/parser/etcd/ES/QD，缺必需依赖应阻断
TEST_AI_SPLIT=true go test -json -p=1 ./internal/bootstrap \
  -run '^TestAISplitRealDependencies$' -count=1 -timeout 8m

# 关键 AI 发布/维护组件的竞争检测；新增交错用例实现后加入其精确名称
go test -race -json -p=1 ./internal/aisystem \
  -run '^Test(AcceptedPublishReceiptAndAtomicContinuations|SupersededStagingAndTombstoneNeverBecomeVisible|MaintenanceIdentityAndGraceProtectAcceptedArtifacts)$' \
  -count=1 -timeout 8m

# 最后一次完整回归：先修复、再记录新快照，只执行一次有意义的全量
go test -json -p=1 ./... -count=1 -timeout 8m
python3 -m unittest parser_worker.test_parsers -v
make proto-check
```

真实链路至少需要 `TEST_POSTGRES_DSN`、`TEST_POSTGRES_READONLY_DSN`、`TEST_CONTROL_POSTGRES_DSN`、`TEST_AI_POSTGRES_DSN`、`TEST_RABBITMQ_URL`、`TEST_RABBITMQ_MANAGEMENT_URL`、`TEST_MINIO_ENDPOINT/ACCESS_KEY/SECRET_KEY`、`TEST_ETCD_ENDPOINT`、`TEST_SEARCH_ES_ENDPOINT`、`TEST_SEARCH_QDRANT_ENDPOINT`，并按服务配置 ES/QD 认证；真实缓存回归另需 `TEST_REDIS_ADDR`。专用 DB 已迁移，测试用户须具备夹具建 schema/角色所需权限，在线角色仍用最小权限账号。

完整核心真实回归还设置 `TEST_ASYNC_CORE=true`；先确认其固定 `async-core-test` bucket 和生命周期配置只落在专用 MinIO。此开关不等于旧长稳或 broker 重启开关。

在 `web/` 下计划执行：

```bash
npm run lint
CI=true npm test -- --runInBand
npm run build
```

浏览器现有 runner 的输入为受保护的 `browser-fixture.json`，不是任意站点 URL。夹具模式须同时安排 `TEST_AI_BROWSER_WEB_ROOT`、`TEST_AI_BROWSER_READY_FILE` 和 runner 生命周期；runner 写 `.done` 仅表示结束，**必须另外校验 `browser-results.json.status=PASS`、检查项及 page_errors**。不能只看 Go 父测试绿就推断浏览器绿。

实际独立 Compose 的 start/stop/upgrade 命令由 E2 runner 在确认项目归属后调用；本节不提供可误落当前业务栈的裸停机命令。新增故障/fuzz runner 尚待实现，不能凭上述现有回归命令认定 A–J 全覆盖。

生成物验证在隔离输出/临时工作副本中比对前后字节，避免覆盖工作区已有修改；OpenAPI 再生成同理。`make proto-check` 若依赖 git 差异，需区分本来已有的生成物差异与再生成引入的新增差异。

## 14. 分阶段执行与退出条件

| 执行包 | 内容 | 完成条件 |
|---|---|---|
| T0 基线/夹具准备 | 快照、资源登记、三库迁移/角色/实际依赖预检；现有关键链路；把全部 ID 展开为待执行记录 | 确认能测的是当前快照；列出所有待补 runner/屏障，不将空跑或 skip 算完成 |
| T1 核心安全与发布 | A01–A07、B 全组、D03–D05/D09–D11/D14、E03/E05、F01–F03/F06 | P0 权限/发布/终态不变量通过，故障窗口都有证据 |
| T2 检索与能力 | D 全组、E 全组、A09；真实 ES/QD/parser 和模型计数 | 各分支正常/故障/覆盖缺失/能力关闭都有独立结果 |
| T3 进程故障与部署 | C 全组、A08、F09；实际独立角色、mTLS、多副本局部接管 | 真实杀进程/专用 broker 恢复/实际 Nginx 与证书完成验证 |
| T4 迁移与清理 | G/H01–H04，真实历史消息、归档 up/down | 数据/任务/job/消息账目守恒，清理保护和重入安全 |
| T5 用户体验与有界并发 | F 全组、H05–H07、性质/随机操作和 race | 无越权/乱序覆盖/预算失控；符合短时上限 |
| T6 最终回归与报告 | 当前快照后端/parser/前端/生成物/配置回归；复查所有 FAIL/BLOCKED/skip | 证据绑定最终快照，保留首次失败和修复后回归；输出结论/限制/资源清单 |
| T7 可选专项 | J01–J03 中环境具备的项目 | 各自独立验收；未执行明确列边界 |

随机操作模型仅组合当前已有的上传、选 scope、search/Ask、重建、删除、移动/重命名、停止/恢复单角色与重投通知；每步检查关键不变量。保留 seed/调度序列/最小失败复现。随机覆盖补充确定性窗口，不代替 T1。

## 15. 验收门槛、证据和最终报告

### 15.1 通过条件

1. P0 全部 PASS；P1 全部完成并有结论，未解决 P1 缺陷/阻塞时只能给“部分完成”，不能给完整验收通过。
2. 无跨 owner 泄漏、staging 裸读、错误版本发布、同世代墓碑复活、computed 重算、无界预算、数据/原消息丢失、无证明删数据。
3. 所有真实依赖用例已验证实际运行；`-run` 未匹配、缺环境 skip、只有 health 成功都不算通过。每条必须验所需顶层测试确实出现且 PASS。
4. 关键恢复窗口有精确故障点、前中后快照、调用数、唯一 receipt、active/后继/job 对账。完整零命中、降级/未支持和失败状态符合契约。
5. 默认配置边界与加速 fixture 分开记录；无把测试短 lease、fake provider、单机多容器的结论扩大为生产 HA/长期稳定/容量/回答质量。
6. 历史失败和本轮失败保留；修复后跑受影响场景，再做一次必要全量。不得删除失败日志或循环重跑到绿后只报最后结果。

### 15.2 记录格式

每例：`case_id / priority / status / environment / source_snapshot / seed / input_ids / preconditions / fault_barrier / expected / actual / provider_counts / task_result_receipt_refs / recovery_elapsed / evidence / residual_resources`。

状态用 `PASS / FAIL / BLOCKED / NOT_RUN / EXCLUDED`；已知缺陷仍为 FAIL 并附 issue。P2 缺资源可 BLOCKED；长时间压测/浸泡属于 EXCLUDED，不能列为“待补验收”。

建议证据目录：`artifacts/ai-deep-validation-<run_id>/`，按 `manifest/`、`results.jsonl`、`backend/`、`browser/`、`faults/`、`migration/`、`reconciliation/`、`resource-inventory.json` 保存。仅含脱敏索引和结论的报告入 docs，受保护原始日志另归档；凭据文件 0600，不提交或公开。

### 15.3 最终交付

- 新增/修复代码与测试、被测快照和配置：与测试执行结果分开列。
- 本轮实际结果：按执行包/场景 ID 汇总，列 PASS/FAIL/BLOCKED/NOT_RUN/EXCLUDED、首次失败、修复及回归证据。
- 实际运行/部署：哪个隔离项目/镜像在跑，哪些进程/代理已恢复；现有业务栈是否变更必须单独写清。
- 保留迁移和清理：数据/任务/job/消息账目、隔离项、临时资源与现场保留清单。
- 结论边界：单机还是跨主机，fake 还是真实模型，哪些格式/能力经过验证；不附容量、长期稳定或完整格式支持承诺。

**本计划书生成时未执行上述测试、故障注入、迁移、部署或真实模型调用。** 当前交付仅为新计划及该文档的 Git 忽略白名单；后续必须按实际执行证据填写验证报告。
