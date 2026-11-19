# PG 简化实施与验证记录

日期：2026-11-19。依据根目录 CONTROL_PG / AI_PG 两份同日计划。原计划继续作为验收基线；本文件区分代码、专用环境证据、实际运行切换和待验收项。

## 交付状态

已实现可显式启用的 v3 直接执行链路及 expand → handover → activate → contract 工具。默认配置仍为 v2，实际 `deploy/.env` 未修改。源码迁移为业务 **37**、控制 **6**、AI **12**；专用库 CLI 的三个版本均为 `dirty=false`。

控制 10 表、AI 15 表已在专用夹具执行收缩 SQL 后计数，并验证收缩后新任务、维护、回执恢复和工具重入。此处不表示任何存量开发/生产库已收缩。迁移工具元数据不计入业务表数。

| 工作 | 当前结果 |
|---|---|
| v3 协议、任务内投递、独立发布/执行凭据、有限回执核对 | 已实现，竞态/最后一次崩溃/只读核对验证通过 |
| 核心直接执行、稳定领域回执、废弃输出定向清理 | 已实现；真实核心 smoke 通过 |
| AI 类型化计算、本地原子发布、稳定任务回执 | 已实现；回滚、赢家复用、旧 seq、墓碑验证通过 |
| 摘要专用节点、写前投影线索、无 results 的作业汇总 | 已实现；节点复用、投影异常、用户作业终局验证通过 |
| 交接、激活、独立收缩、数据库写者 fence | 已实现；专用 SQL 夹具及空库 CLI 重入通过 |
| build、vet、全量回归、相关包 race、真实依赖 smoke | 结果见下文和 artifacts 证据 |
| 控制协议及实际 AI 阶段的 SQL/事务/WAL 对照 | 已实测，范围是串行微基准；完整负载验收仍待执行 |
| 配置、角色权限、管理工具、Docker 构建与启动接线 | 已同步；Compose/脚本静态检查通过，未构建部署镜像 |
| 真实存量分类、迁移批次归档、运行环境切换 | 未执行；不能把专用夹具作为真实存量交接完成的证明 |

## 代码和迁移

`asynccontrol/direct*.go` 实现任务投递合并、条件领取、完成、退避、失租恢复、一次回执核对和未知结论停止。普通成功 worker 使用一次领取事务与一次完成事务；长任务续租、领域延期、失败、后继和清理成本另计。完成描述只含任务身份、输入摘要、outcome 和有界引用，不保存完整计算值。

`asyncjobs/direct.go` 及共享领域提交保留语义锁、配额、引用、会话终局和对象世代检查。新回执以稳定逻辑任务身份识别；首次提交拒绝采用输出以及并发失败方命中赢家回执时，都在领域事务登记未采用输出的清理意图。控制重试/失租在轮换凭据之前，按冻结输入和旧代次登记有限定向清理任务。外部 I/O 不进入事实事务。

`aisystem/direct.go` 接收当前进程的类型化计算值，在 AI 本地事务内写完整规范化成果、切 active 指针、写稳定回执。新任务不写通用 artifact，不调用 SaveResult/BeginCommit/AcceptedResult。同一任务不同计算值返回已提交赢家；多条历史回执或输入冲突拒绝采用。文本切换清空不兼容向量指针，旧 seq/依赖/墓碑不能覆盖当前成果。

摘要节点迁往 `ai_summary_nodes`，节点身份不含执行代次，保持 1 MiB 节点边界和模型调用预算。ES/Qdrant 外部写前保存 `ai_orphan_projections` 清理线索；覆盖证明和回执同事务发布。写者期限、安全宽限、active/retired 引用约束物理清理，仍需独立 projector 完成删除后才回收相关规范化元数据。

控制完成事务先登记必要子任务及作业关联，再汇总用户作业。作业引用来自 `ai_job_task_links.completion_refs`，v3 不 JOIN results。最终回执缺失/不可查时，控制和用户作业均保留 `needs_attention`，不会把未知提交状态报告成确定失败，也不会让未生成的后继永久挂起。

新增迁移：

- 业务 `000037_direct_receipts`：稳定回执的协议、任务及效果身份。
- 控制 `000006_direct_execution`：内嵌投递、回执核对字段、索引及 v3 写者 fence；扩展期旧 shard 列补默认值，使收缩前后使用相同新插入路径。
- AI `000012_direct_publication`：摘要节点、稳定回执索引、对象写协议及投影清理字段。
- 控制 `control-contract-migrations/000001_archive_v2`：显式检查旧结果/后继/输出/observers 等证据，再移入 `async_v2_archive`，保留任务快照并退出旧字段。
- AI `ai-contract-migrations/000001_archive_artifacts`：只能经 `ai-direct-admin archive` 跨库预检后执行；移入 `ai_v2_archive`，私有摘要视图改读专用表。

历史迁移 SQL 未改写。收缩保留历史表和证据，不物理删除历史数据；启用 v3 后不支持用旧镜像/down migration 解释新任务。

## 交接和启动

`async-handover` 按 ASYNC_PROTOCOL_VERSION 选择 v1→v2 或 v2→v3；v3 不重新运行核心 async-import。旧已接纳未提交结果、未知命令提交和 blocked 后继会阻止自动转移，必须先在兼容 v2 窗口处理。停止结论、耗用计数和原期限保留；旧通知先持久记录处理证据，再 ACK，不 purge。

`ai-direct-admin` 提供 inspect / transfer / activate / archive。transfer 校验并搬迁原摘要节点，不调用模型替代旧节点；不能证明身份的节点保留并报错/计入 retained。归档要求通用 artifact 有匹配的已提交回执，或原摘要节点已核验迁移；未接纳的未知物理输出不能仅凭年龄归档。历史迁移批次、observers 或接受结果存在未决依赖时保留兼容表。

准备实际切换时，唯一配置仍为 `deploy/.env`，协议配置应成组变更，例如：

```dotenv
ASYNC_PROTOCOL_VERSION=3
ASYNC_BROKER_PREFIX=bx.async.v3
ASYNC_LEGACY_BROKER_PREFIX=bx.async.v2
```

旧前缀应填写真实旧值。`deploy/up.sh` 在停止服务前检查 v3 前缀；随后停旧写者、执行扩展迁移、控制交接、摘要节点交接和 AI 对象激活、控制激活，最后启动显式角色。有效租约或历史证据未就绪时会拒绝继续。首次从 v1 升级必须先走 v2 交接。

显式收缩入口（必须在存量核验及停写条件满足后操作）：

```bash
AI_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh ai-direct-admin archive
./deploy/local-run.sh migrate control-contract up
```

普通 `migrate up/control up/ai up` 和 `deploy/up.sh` 均不自动执行收缩。Docker runtime 已复制两组 contract SQL；Compose/build 脚本提供 ai-direct-admin。query 继续限于发布视图，不能读写私有摘要节点；AI worker 继续无业务 SQL 池，API 继续不依赖控制 PG/MQ。

## 实际验证

工具链为 Go 1.25.13 linux/arm64，使用专用业务/控制/AI 数据库和隔离 schema、MQ vhost、MinIO bucket、投影命名空间。运行时依赖是 PG17/pgvector、RabbitMQ 4.1、MinIO、etcd 3.6.6、ES 8.19、Qdrant 1.15.5；parser 为真实 Python 子进程，模型为 fake provider。

- 全部 cmd 入口逐个 `go build -p=1 -o /dev/null`；使用逐入口构建避免受限磁盘同时保留几十个链接产物。
- `go vet -p=1 ./...`。
- `go test -json -p=1 ./... -count=1`：42 个包通过，965 个测试通过，18 个测试跳过，0 失败；另有 38 个包无测试文件。原始事件与逐项 skip 明细保存在证据目录，skip 不计为通过。
- `go test -race -p=1 ./internal/asynccontrol ./internal/asyncjobs ./internal/aisystem -count=1`。
- `TestAsyncCoreDirectRealDependencies`：真实 HTTP/PG/MQ/MinIO 上传、分享引用与配额、hash 错误、取消、过期/延期和授权 GC；API 在控制/MQ 不可用时仍接收请求。
- `TestAISplitDirectRealDependencies`：真实三库/parser/etcd/ES/Qdrant 链路、权限隔离与投影异常；模型为 fake provider。
- 新协议 focused：晚 confirm、过早通知、失租发布者、重复通知、最终只读核对、后继原子回滚、回执复用/冲突、旧 seq、墓碑、摘要节点复用、用户作业 needs_attention。
- 保留数据夹具：v2→v3 转移重入、预算/期限保持、旧 v2 消息证据及旧写者拒绝；历史未决结果按拒绝/保留路径处理。
- 全量之后修复旧 running 任务失租交接时未转为 pending 的问题，并新增旧摘要节点迁移后不重调模型的用例；最终 `^TestDirect` 在 asynccontrol/aisystem 两包以 race 通过。最后再次执行 vet 和 async-handover 构建通过；没有把修复前的全量记录写成修复后的全量重跑。
- 收缩夹具：控制 10 表、AI 15 表，收缩后新任务/维护/工具重入。
- 新建专用空库执行实际 CLI：业务 37、控制 6、AI 12，均 dirty=false；交接/激活/收缩/收缩后重入成功。空库工具验证不代表大规模真实历史数据验证。
- 两个 env 模板键集合相同，新增变量在 Go/Compose/两个模板齐全；Compose 全配置、相关 bash 语法、格式化及 diff 检查。

未改前端和 parser 行为；未执行付费模型质量、OCR/额外文件格式、跨主机 HA 或容量认证。未开启/缺环境的可选集成 skip 与真实链路通过分开记录。

## 实测读写对照

在新建独占 PG17 实例启用 pg_stat_statements，同一扩展 schema 分别运行 v2/v3，串行输入相同，不重置其他数据库的全局统计。以下 WAL 为 SQL 语句归因的 wal_bytes，显式事务为 COMMIT 计数，**不包含 autocommit 的隐式事务**。

控制协议微基准每类 20 个样本，覆盖 ensure/投递、发布持久化、领取和完成。普通计算类的汇总：

| 每任务指标 | v2 | v3 |
|---|---:|---:|
| SELECT | 8 | 6 |
| INSERT | 5 | 2 |
| UPDATE（核心） | 11 | 5 |
| 变更行数 | 16 | 7 |
| 显式事务，包含 ensure / 发布领取 | 6 | 4 |
| worker 显式事务 | 4 | 2 |
| multipart.complete WAL bytes | 6747.9 | 3616.7 |
| upload.verify WAL bytes | 6765.9 | 3622.4 |
| canonical.ensure WAL bytes | 6818.6 | 3620.8 |
| thumbnail WAL bytes | 6712.5 | 3562.6 |

普通命令 upload.expire 的 worker 原本就是 2 个显式事务，v3 仍为 2；控制总变更行数 10→7，WAL 4212.9→3633.8 bytes。AI 类完成多一次 completion_refs UPDATE 查询；无关联作业时该查询不变更行。此微基准不执行真实字节/模型/后继/作业工作，仅计量协议成本。

另执行实际 source/text/vector/summary 规范化写入，每协议 3 组相同短文本、相同 fake model；source 使用相同预先解析值，不计 parser I/O。结果如下（每阶段每样本）：

| AI 阶段 | AI PG 显式事务 v2→v3 | AI PG WAL bytes v2→v3 | 控制 PG WAL bytes v2→v3 |
|---|---|---|---|
| source | 2→1 | 3649.7→2848.3 | 10372.0→6090.0 |
| text | 2→1 | 11366.7→9901.3 | 9876.7→5874.3 |
| vectors | 2→1 | 33730.0→31254.7 | 9928.7→5880.7 |
| summary | 3→2 | 6080.7→4269.7 | 9865.0→5824.7 |

摘要仍需专用节点写入，因此保留额外事务。AI 各阶段的控制变更行数为 16→7，控制显式事务 6→4；AI 自身变更行数减少 1，源于退出通用 artifact。收益随文本/向量大小及压缩而变化，不能将这些短文本数字外推成容量结论。

尚未达到原计划完整负载性能验收：Relay/后台空转/维护/续租/AI 后继和真实作业汇总、重复通知/回报失败/失租重算各路径的全系统对照，持续并发锁等待、连接峰值、内存峰值以及外部投影实际 I/O 对照仍需在专用负载环境完成。当前微基准不覆盖这些成本，也不宣称全面性能验收已完成。

## 运行状态与剩余边界

- 变更留在当前工作区，未创建 commit。原有 CODEBASE_MAP.md 改动保留；未覆盖实际 `.env`。
- 仅变更专用测试资源；未启动新生产角色、未构建/发布镜像、未切换存量业务库/控制库/AI 库。
- 未 purge 队列、未删除用户卷/业务对象、未丢弃历史已接纳结果；归档是保留证据的 schema 移动。
- 最终 10/15 表为专用验证结果。真实历史 artifact/observers/迁移台账的清单、未决项核对和真实数据量下的迁移锁耗时未验证，不能自动宣告归档条件满足。
- v2 兼容代码及其配置/权限依赖保留。全仓不可能在同时支持 v2 的阶段“没有任何 results/artifact 引用”；保证的是 v3 新任务在线分支退出这些读写。真实存量依赖清零后才能进一步删除兼容代码与账号。
- 原计划的完整性能/真实运行切换验收仍待执行；这些限制不由单机 smoke 或 fake provider 结果消除。

原始日志、微基准和包级/skip 汇总位于 `artifacts/pg-simplification-20261119/`；`validation-summary.json` 汇总实际结果和未验收项，`sha256.json` 记录证据文件校验值。该目录按仓库惯例忽略，不包含测试 env 或密钥，复制日志时已脱敏。
