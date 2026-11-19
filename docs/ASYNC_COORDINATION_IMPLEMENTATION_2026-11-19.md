# 异步协调裁剪：实现、验证与恢复

日期：2026-11-19。依据根目录 `ASYNC_COORDINATION_SIMPLIFICATION_PLAN_2026-11-19.md` 执行。代码与配置保留在工作区；没有切换生产数据库、运行生产迁移或启动生产新角色。11-16 施工书是历史基线，两个实现说明和 AGENTS.md 已同步 v2。

## 1. 已实现

默认链路现在是 outbox → 控制 PG 原子接管/投递意图 → 工作 MQ → worker 计算、接纳 checkpoint、领域事务、控制回报 → ACK。核心 worker 自己写业务 PG，AI worker/projector 自己在 AI PG 发布。两个 committer 入口拒绝启动，退出默认构建、Compose 和观测目标。

| 部分 | 具体变更 |
|---|---|
| 控制协议 | 新增 control 000005；pending/running/done/stopped + outcome/phase；task_deliveries 管理发布 claim、确认和有限发送；没有结果队列或第二套重试权威 |
| 裁剪 | 删除结果消费/补发、分片成员和租约/写许可、跨任务聚批及公平/优先级/高水位调度。历史表和预算字段保留，不删除用户数据 |
| 计算与提交 | 接纳 checkpoint 保留 token、结果身份/代次/摘要，提交前复核接纳证明；每任务独立短事务及 receipt；已接纳结果恢复时不重算 |
| 故障窗口 | 领域已提交而回报丢失可复用 receipt；ACK 丢失读取持久终态；最后提交崩溃有有界 receipt-only 恢复；无法证明则保留结果并停止 |
| 纯命令 | upload.expire/object.authorize_delete/failure.converge/ai.job.finish 进入 core-command；gc-worker 单并发消费。无外部计算的命令使用稳定 receipt 身份，无伪造 checkpoint |
| 核心领域 | 保留 multipart 未知成功恢复、hash/大小验证、安全复制、配额/引用/epoch、缩略图及对象世代 GC。外部 I/O 不进入业务事务 |
| AI | 保留 manifest 校验、active/desired/墓碑、summary checkpoint、embedding cache、投影证明和 PG 降级。接纳时冻结后继，领域成功后的控制回报事务才释放并汇总作业 |
| 维护 | gc-worker 承接业务 outbox 留存、会话到期扫描和 staging 生命周期初始化；控制侧保留输出定向清理及有限归档，停止预算和未提交结果不归档 |
| 权限 | 核心 worker 独立业务写池；协调角色仍业务只读；AI 后台用 AI_COMMIT_DSN，query 仍只读发布视图，AI 无业务 SQL/迁移账号 |
| 配置/运维 | 移除退役在线参数，两个模板键一致；status/config-check、Prometheus、admin 和测试夹具同步。worker 指标不再各自全表汇总任务 |

拓扑默认 bx.async.v2，共 11 个工作队列和 dead。消费者并发/prefetch 只约束本消费者，扩副本会增加总量；没有旧的全局写并发/速率或 owner 公平保证。控制、业务、AI 各库使用本地事务、至少一次通知和幂等重放，没有跨库分布式事务。

## 2. 预算与连接边界

新任务保留计算、发布、提交上限及期限；提交上限沿用 ASYNC_MAX_ATTEMPTS，提交窗口单独使用 ASYNC_COMMIT_WINDOW。失租计入已领取尝试，发布重试沿用累计 dispatches。已确认排队的通知不周期重发；早到通知只有重新持久化未来投递后才 ACK。

历史 max_recoveries/result_sends 仅供交接收紧剩余机会。旧恢复预算已耗尽时停止并登记有限失败收尾，不按新默认值重新计算。已接纳结果保留原 ID/digest/代次，提交重试不改变身份；停止任务保留原原因与预算，redo 另建审计子操作。

令 P=POSTGRES_MAX_OPEN_CONNS，C=ASYNC_CONTROL_MAX_OPEN_CONNS，R=ASYNC_CONTROL_CRITICAL_CONNS，N=ASYNC_WORKER_CONCURRENCY：

- 核心 worker：控制 C + 保留 R + 业务读 min(P,max(2,N)) + 业务写 min(P,max(2,N+1))。GC 的命令与维护复用该角色的池。
- 协调角色：控制 C + 保留 R + 业务读 min(P,max(2,N))。
- AI 后台：AI min(P,6) + 控制 min(C,6) + 保留 min(R,6)。query/core-internal 不打开控制池。

总连接量要按实际副本数求和；这只是池上限核算，不是容量测试或数据库资源隔离证明。独立保留池继续服务续租、checkpoint 和完成回报。

## 3. 存量交接与恢复

新增 async-handover inspect/transfer/activate。transfer 要求旧写者已停止，拒绝尚有旧执行/模块租约的控制库。按小批同事务保存原任务快照、保守预算、转换状态及首条投递；中断重跑不产生新语义身份或预算。

旧 MQ 每条在 coordination_messages/隔离记录保存队列、原文 base64、摘要与处理结论后才 ACK；有消费者时拒绝排空，不 purge、不删队列。超过 1 MiB 的历史消息保持未 ACK，交由人工检查。旧 result 通知必须与保留结果身份相符才能认定完成交接。

```bash
# 先停全部旧核心/AI 写者，等待旧租约到期；在同一 deploy/.env 上执行
./deploy/local-run.sh migrate control up
ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-handover inspect
ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-handover transfer
ASYNC_LEGACY_WRITERS_STOPPED=true ./deploy/local-run.sh async-handover activate
```

旧拓扑默认 ASYNC_LEGACY_BROKER_PREFIX=bx.async.v1；自定义旧前缀须显式配置，新的 ASYNC_BROKER_PREFIX 必须不同。新角色启动检查 v2 激活标记，控制库 trigger 阻止旧连接修改已切换任务。

up.sh 停新旧写者后，迁移/初始化、交接/激活，再启动 v2；按项目和 service label 停止原 Compose 遗留 committer 容器，保留容器和卷。已有 v1 控制库不再重跑核心 async-import，也不自动重复 AI 历史 prepare/archive。真正从旧业务执行账本升级时，仍单独走原 async-import 流程。

控制 5 的 down 明确拒绝逆转协议。发生故障应停止新写者、保留三库/MQ/结果及 receipt，核对 async-admin inspect 和交接快照后向前修复。旧镜像不能作为已切换协议的回滚方案；本轮没有认证跨库备份恢复或旧格式反向转换。

## 4. 已执行验证

所有集成使用 bx-coord-v2-test-* 专用依赖和随机 schema/vhost/bucket/namespace；没有把套件指向开发或生产库。SQL 夹具应用业务 1–36、控制 1–5、AI 1–7；夹具直接执行 SQL，不能据此宣称 public 的 schema_migrations version/dirty 已被更新。

完整记录保留在 `artifacts/async-coordination-20261119/`（被 Git 忽略），包括最终日志、JSON 汇总及中间失败日志，不包含测试环境文件或用户密钥。测试计数包含子测试；有测试执行的 package 和无测试文件的 package 分开统计。

| 已执行项 | 实际结果 / 证据文件 |
|---|---|
| 后端全量 `go test -json -p=1 ./... -count=1 -timeout=8m` | 退出 0；955 项 pass、0 fail、12 skip；42 个有测试的包通过，另 37 个包无测试文件。`bx-coord-v2-final3-tests.jsonl`、`bx-coord-v2-full-summary.json` |
| 最后增加提交前 lease/token/generation 校验后的受影响包回归 | asynccontrol、asyncjobs、aisystem、bootstrap 四包退出 0；253 项 pass、0 fail、4 skip。`bx-coord-v2-guard-tests.jsonl`。随后仅格式化代码，没有再次修改行为 |
| 核心真实依赖冒烟 | `TestAsyncCoreRealDependencies` 全量轮 3.10s；最后四包轮 3.49s。贯通 HTTP/MQ/MinIO、上传/校验/复制/缩略图/配额/引用/GC，无独立 committer |
| AI 真实依赖冒烟 | `TestAISplitRealDependencies` 全量轮 10.07s；最后四包轮 10.34s。真实 parser/etcd/MQ/MinIO/ES/Qdrant，模型为 fake provider，无独立 ai-committer |
| 最终格式、构建、静态分析 | 变更 Go 文件 gofumpt/goimports 完成；`go build ./cmd/...`、`go vet ./...` 均退出 0。`bx-coord-v2-closeout-checks.json` 及对应日志 |
| 最终竞态检查 | `go test -race -p=1 ./internal/asynccontrol ./internal/asyncjobs -count=1 -timeout=8m` 退出 0；两包分别 3.668s、5.751s。`bx-coord-v2-closeout-race.log` |
| 控制迁移实际 CLI | 新专用库 `coord_v2_migrate_ab587f78` 上运行 `control up` 两次及 `control version`，均退出 0；version=5、dirty=false。`bx-coord-v2-migration.log/json` |
| v1 → v2 实际 CLI 交接 | 专用库 `coord_v2_upgrade_b1e9dbe1` 先迁移 1–4 并写旧夹具，再执行 5；inspect、transfer 两次、activate 两次及最终 inspect 均退出 0；最终 5 clean。`bx-coord-v2-upgrade.log/json` |
| 配置、部署文件静态检查 | 实际 `.env` 和两个模板各渲染全 profile Compose，46 个服务，均无两个 committer；检查核心写池、控制只读及 AI/query 连接边界。模板各 267 个唯一键，键集一致；5 个 shell 文件语法、3 个 Python 文件 AST、`git diff --check` 通过。`bx-coord-v2-static-checks.json` |

交接实测中，原 computed checkpoint 的 UUID、generation=3、attempts=4、dispatches=5、recoveries=1、result_sends=2 均保留；原 needs_attention 映射为 stopped/needs_attention，attempts=12 未刷新。交接快照恰为 2 条，重复执行没有新增快照或重算 checkpoint。

全量轮的 12 个 skip 为：可选中文 parser pipeline 1 项、真实 Redis 相关 4 项、真实模型图片链 1 项、破坏性 broker restart 与长稳开关 2 项，以及 4 个供父测试启动子进程的 helper 入口。它们不计入上述 pass；四包轮的 4 个 skip 是其中 Redis 3 项及真实模型图片链 1 项。精确名称见 JSON 汇总。

专项覆盖包含：安全水位/原子接管，重复来源/消息、多 worker 竞争，confirm 丢失和晚回写，接纳后崩溃不重算，最后提交崩溃的 receipt 恢复，旧 token/代次拒绝，有限失败收尾，真实旧 MQ 排空中断/重入与先证据后 ACK，原文隔离保留和数据库旧写者 fence。

完整 AI 冒烟实际使用 Python parser、etcd、RabbitMQ、MinIO、ES、Qdrant 和 fake provider，贯通上传到内容/向量/概览发布、投影、检索/问答及用户作业。fake provider 不证明付费模型质量，也不保证模型在未知成功窗口只调用一次。

历史失败保留：首次完整 AI 冒烟因宿主机缺 defusedxml 等 parser 依赖失败，在临时 venv 安装既有 requirements 后通过；中间回归暴露崩溃子进程缺控制连接、投影重试夹具未更新投递时间和移除 Commit 后跨包用例仍调用旧接口，均已适配。旧账本兼容冒烟把按设计拒绝的 multipart.sweep 计作异常，现明确只允许该操作的 skip-retry，其他错误仍失败；当前核心 GC 专项实际执行 sweep。第一次专用 PG 512 MiB tmpfs 耗尽后，只替换了本次可丢弃测试容器并增至 1200 MiB；未清理用户缓存/镜像/卷。

## 5. 实际部署状态与未验证范围

未修改 deploy/.env、生产业务/控制/AI 数据库、现有业务 bucket 或数据卷。生产服务在本轮开始前已停止，本轮没有将其升级或启动。没有执行新镜像全栈构建/生产 rollout，也没有将静态 Compose 检查写成部署通过。

本轮启动的专用测试依赖仍保留：PG :25439、RabbitMQ :25679/25680、MinIO :29009、ES :29209、Qdrant :26339、etcd :22389；均为单机临时资源。测试工作进程退出后仅这些依赖容器运行，不清理其他资源。测试 parser 使用 /tmp/bx-coord-parser-env，与宿主机环境隔离。

前端、HTTP/proto/OpenAPI 和 parser 实现未改，不新增其行为验收。deploy/tests 的 Python 进程/容器脚本已适配并做语法检查，本轮没有实际执行这些完整脚本；真实链路证据来自 Go 集成用例。Redis 专项、长稳/破坏性 broker 重启开关按实际 skip 单列。

跨主机 HA、PG/MQ 集群容灾、容量、长压测、大表迁移耗时、跨库备份时间点恢复、TLS/mTLS 实际部署和付费模型质量均未认证。14 天生命周期没有做等时等待验证；旧表、旧字段、旧队列和原结果均保留，物理清理不属于本轮。
