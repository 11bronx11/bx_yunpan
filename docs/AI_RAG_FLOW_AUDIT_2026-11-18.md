# AI 拆分后 RAG 与异步流程复核（2026-11-18）

本轮基线为 `1cd297b574d7df8a47424074c037cac767ca7439`。继续对照保留的旧 `internal/ai/search.go`、`rag/typed_chunk.go` 与当前 `aisystem`、角色装配，检查 RRF、rerank、top-k、普通/指定文件 RAG、父子切片及异步发布。今天较早的补齐记录见 `AI_SPLIT_PARITY_AUDIT_2026-11-18.md`；其测试数字不作为本轮通过证据。

前端没有改动或执行测试/构建。当前业务栈未部署本轮代码，用户 `deploy/.env` 未修改；没有对现有业务库执行迁移、批量重建或清理用户索引/数据。

## 发现与修复

| 项目 | 可复现问题 | 本轮实现 |
| --- | --- | --- |
| RRF 与小 top-k | 新 Query 把最终 limit 直接用于两路召回；limit=1 会在融合前丢掉共同相关候选。旧版 hybrid 至少召回 50 项 | hybrid 每分支至少保留 50 项；分批仍先按原始分数全局合并，再按 `1/(60+rank)` 融合。固定文本/向量排名验证共同 rank-2 命中胜出，分值为 `2/62` |
| rerank 配置 | 新角色没有装配 `AI_RERANK_CANDIDATES`、`AI_RERANK_TOP_K`；内部硬编码前 100 项重排，余下候选还可能进入答案 | 两个原有配置接入 Query。先形成有界候选池，再对整个池重排；证据父片数同时受配置和 scope 硬上限约束。不存在未重排的尾部混入 |
| 指定多文件候选池 | 原 240 项截取把每个文件首片搬到前面，破坏融合排序；固定只重排 100 项又截断剩余文件机会 | 先预留相关文件候选，再按全局相关性补满；被选候选保留原排名。别名按物理内容去重，最终仍分别回验合法逻辑文件 |
| 标题上下文 | 父子片只保存 heading 元数据，PG/ES、embedding、rerank 和父片证据忽略标题；“数据库备份 三份”不能召回正文“每次保留三份” | AI 迁移 000009 回填含标题的 FTS；新 ES schema 包含同样的标题词。新 v3 embedding 的输入和发布 hash 包含有预算的标题前缀；rerank、父片扩展也带标题，原始正文与精确 child→parent 范围保持原值。DOCX 表格继承所属章节 |
| 表格切片与引用行号 | 长末行切开后产生只有表头的额外片段；单列表格的 `---` 分隔线被当作数据行 | v3 跳过空尾片并识别单列分隔线。超长行的各片仍引用原行、保留表头，父/子 token 上限继续校验 |
| 定向重建状态 | summary-only apply 会把同序号的失败 vector 状态重新写成 pending，但没有安排 vector 任务 | 同序号 terminal failure 不接受 pending 覆盖；新 seq 仍可重建，成功版本与迟到失败的保护继续保留 |
| 投影删除确认 | ES HTTP 200 内的 timeout、version conflict、逐项失败被忽略；QD acknowledged 也可能被当成已删除 | ES 校验完整删除响应；QD 要求同步 completed。失败时不生成成功清理结果、不遗忘清理事实；目标 404 继续视为幂等删除成功 |

## RAG 流程与预算

实际入口为 `aisystem.Query.Ask`：core 打开范围并分页 → 只解析范围内 active content/set IDs → 文本/向量独立召回与降级 → 每分支合并全局排名 → RRF → 有界候选池 → core 回验 → rerank → parent 去重扩展 → 估算 token 预算 → core 回验 → Answer → 返回前回验及版本引用。

- 普通 all/folder 在当前有权且 text-ready 的文件中召回，证据采用软文件多样性。源码 scope 默认预算为 8000 tokens、最多 12 个 parent。
- selected 先验证明确文件列表，最多 50 个，不在失败时扩大范围。多文件每个物理内容每分支最多取前 8 项作为起点，最终全局池上限 240；证据共用 12000 tokens、最多 24 个 parent。相关文件尽量分得证据，未进入证据的文件由 coverage 明示。
- 两个既有 rerank 配置现在实际生效：配置候选数再受 240 上限约束，配置 top-k 再受 12/24 个 parent 上限约束。当前默认配置值为 30/8；本轮未修改默认值。配置与 token 预算都可能使所选文件不能全部进入证据。
- 召回候选数、最终文件数、重排候选数和答案 parent 数分别控制。Search 的 limit 不再提前破坏 hybrid 召回；Search 保持旧版的检索接口职责，外部 rerank 用于 Ask。
- reranker 失败保留融合候选并标注降级；无兼容 query embedding 的 semantic 明确不可用，hybrid 可继续文本分支。fulltext 不调用 embedding/reranker/chat。
- 单文件没有每文件两片硬限制。命中多个 child 时只展开一次相同 parent；模型前后都回验，范围外正文不能进入 reranker 或 answer。

## 版本与发布边界

默认切片版本升级为 `parent-child-v3`。v1/v2 的生成路径、ID、范围及旧 embedding 原始输入 hash 保留，parser 仍对这些版本只返回规范源块，避免重新执行旧的 parser 端切片。

新 ES target digest 使用 `text-schema-v3:heading-context:<lexical version>`；已接纳的 v1/v2 ES manifest 仍可发布 receipt，但 reader 要求当前 schema 的覆盖证明，否则回退 PG。新投影 hash 包含实际索引的标题上下文，QD 旧 hash 结构保持兼容。

迁移 000009 只重建 FTS，不重解析、不调用 embedding。它的 up/down 已在独立测试 schema 验证；现有业务栈尚未执行。该迁移会更新全部已有 child 的索引数据，大数据量时的写入耗时与锁等待未实测。现有向量和旧父子片保留，要取得 v3 的标题向量与表格修正，需要显式重建相应能力；本轮没有自动重建全部对象。

worker 仍只插 staging；控制库仍为唯一执行权威。ai-committer 在本地事务内验证 manifest、依赖版本、CAS/墓碑并写 receipt，之后回报控制库释放后继。没有业务 SQL 直连 AI、跨库 JOIN 或额外执行权威。

## 本轮执行证据

证据根目录为 `artifacts/ai-flow-audit-20261118/`，凭据文件为 0600，处于 Git 忽略范围。Go 使用 1.25.13、GOMAXPROCS=2、`-p=1`。沿用专用 parity 三库，每个夹具继续隔离 schema；MQ/vhost、MinIO bucket、ES/QD target 由夹具隔离。只恢复此前停止的专用 ES/Redis 测试容器，不调整用户 ES 配置或运行中的业务服务。

| 验证 | 实际结果 |
| --- | --- |
| 首轮 AI 定向回归 | `review/parity-1-summary.json`：272 pass / 0 fail / 1 skip；真实模型图片专项未启用 |
| RPC 治理 | `review/parity-2-summary.json`：1 pass；实际 socket 的请求 ID、deadline、重试、认证、panic 和 metrics |
| 新排名用例 | 小 top-k 的跨分支共识、all/selected 下 3 个 rerank 候选与 2 个证据限制、rerank 失败降级、指定文件份额、范围隔离与 parent 去重 |
| 跨范围批次 | `late-batch.log`：201 个已发布内容版本，最后一批的高原始分数成为全局第一；同轮 rerank 故障回退通过，2 pass |
| 标题与迁移 | 规范源 → text → embedding 发布 → rerank → parent Answer；真实 ES/PG 同查询正反例；000009 down/up 保留正文并重建召回；旧投影接纳兼容 |
| 表格边界 | 超长末行、单列表格、v2 冻结输出、现有父子覆盖/offset/token 属性验证 |
| 父片去重与长文预算 | `verified/budget-summary.json`：3 pass；10 个长 parent、每个有多个命中 child，验证只展开一次、使用同一发布版本的完整正文、普通/selected 证据分别触发 8000/12000 上限，计数与实际模型输入一致；同用例也通过 race |
| 完整服务链 | 本轮 `TestAISplitRealDependencies` 已 PASS：HTTP 上传 → MinIO/MQ/三库 → parser → text/vector/summary → ES/QD → Search/Ask/引用及重建；包含生产角色装配 |
| Python parser | `verified/parser-summary.json`：17 pass / 0 fail / 0 skip，新增 DOCX 表格章节断言包含在原有用例中 |
| build / vet | 全量及新增测试完成后按最终源码重新执行；`verified/build-vet-1-summary.json`、`build-vet-2-summary.json` 退出码均为 0，分别耗时 181.60 / 11.79 秒 |
| 最终全量 | `verified/all-summary.json`：815 pass / 0 fail / 7 skip，退出码 0，313.99 秒 |
| 定向 race | `verified/ranking-race-summary.json`：41 pass / 0 fail / 0 skip，退出码 0，65.38 秒；覆盖新排名、指定文件、标题、迁移、投影删除、父子片与 socket RPC 治理 |

首次基线因恢复 Redis 后宿主端口变化，`TestQueryEmbeddingCacheRealRedisAcrossInstances` 连接旧端口失败；只修正本轮私有测试环境，随后定向轮通过。`reproduced.log`、`heading-reproduced.log`、`delete-reproduced.log` 保存修复前的实际失败。表格新增边界用例还暴露了单列分隔线问题，修复后对应三项定向验证通过。

第一次全后端回归记录在 `final/`：809 pass / 2 fail / 7 skip。两个失败为非 AI 的 `TestStorageLeaseRenewsAcrossRecoveryIdentities`（数据库认证阶段超时/失租）及 `TestBatchConfirmationAndFrozenReplay`（时间戳断言）；该轮与构建并行，不能据此认定唯一根因。失败原件保留，构建结束后串行复验两项均通过；没有放宽核心租约/超时，也未修改上传代码。

全量通过后仅新增上述长文预算测试，随后单独执行及定向 race 均通过；没有修改生产 Go/Python/SQL 代码。各轮 `*-source.json` 保存执行时源码 SHA-256 清单，计数包含 Go 子测试，不将不同轮的通过数相加。7 个 skip 包含未开启的真实模型专项、4 个供父测试单独调用的子进程辅助入口、未开启的破坏性 broker 重启和长时稳定性测试；不把后两项计为已验证。

`boundary-check.json` 核对开始时记录的 76 个前端文件哈希全部相同，无新增前端文件，`deploy/.env` 哈希相同。parser 测试生成的两份已跟踪 Python bytecode 已恢复为基线；本轮保留改动仅涉及 AI 后端、迁移与报告。

两份专用测试容器 `bx-ai-parity-es-20261118`、`bx-ai-parity-redis-20261118` 已在依赖验证结束后停止，状态为 exited，容器和数据保留；专用三库也保留供复核。最终资源记录见 `owned-resources.json`。未重启现有业务服务或修改用户数据。

本轮模型调用采用确定性 provider 与 HTTP 响应夹具，未重新调用付费真实模型，不能据此宣称 reranker 的语义准确率、模型回答质量或复杂文档全集正确。普通 RAG 仍不是全文穷尽分析；无新生产容量、长期稳定性或跨主机 HA 结论。
