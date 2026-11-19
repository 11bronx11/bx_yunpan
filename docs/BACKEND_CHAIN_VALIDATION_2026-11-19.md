# 后端用户链路与白盒核验

日期：2026-11-19（北京时间）。本轮验证当前工作区；基线提交及开始时间见 `artifacts/backend-chain-20261119/run-context.json`。

## 范围与环境

先执行真实 HTTP 用户链路，再执行后端全量回归、数据库与外部索引对账、故障恢复和竞态检查。覆盖默认 v2 与显式 v3，不修改实际 `deploy/.env`，不切换存量运行协议。

本轮业务、控制、AI 数据库分别为 `chain_20261119_business`、`chain_20261119_control`、`chain_20261119_ai`，从已有专用测试库建立，与开发业务库分开。迁移版本分别为 37、6、12，均 `dirty=false`。用例进一步使用独立 schema、MQ vhost/队列前缀、MinIO bucket 或对象 key、ES/Qdrant 目标命名空间。

Go 为 1.25.13 linux/arm64。PG、RabbitMQ、MinIO、etcd、Elasticsearch、Qdrant 和 Python parser 为真实依赖；HTTP 矩阵使用 fake provider 以固定模型输出，另执行有界真实 DashScope 模型链路。应用角色由实际 bootstrap 在测试进程内启动，角色间通过真实 HTTP/gRPC/MQ 通信；这不是 Docker 应用镜像部署验收。

## 黑盒请求与预期

| 请求或场景 | 预期与核验 |
|---|---|
| 注册、登录、获取根目录 | 201/200，身份与目录可用 |
| 控制 PG/MQ 不可用时创建上传 | API 仍接收请求，业务 outbox 已提交，后台恢复后继续 |
| 创建 → presign → 直传 → confirm → complete | 201 → 200 → MinIO 200 → 200 → 202，轮询最终 completed |
| 下载与缩略图 | 下载字节完整一致，缩略图 ready，staging 清理完成 |
| 分享创建、公开解析、其他用户导入 | 201/200/201，共享物理对象、引用数与双方逻辑配额正确 |
| 分享撤销、到期与并发版本 | 撤销后旧 token/resolve 404，已导入文件保留；更新推进 version，旧版本 PATCH 409；实际等待 2 秒到期，旧 token 关闭、过期分享不可重开 |
| 声明 SHA 与实际内容不同 | 会话 failed，只释放失败上传的占用，共享原对象不受影响 |
| 取消会话 | DELETE 204，reserved_bytes 归零；晚到 multipart 结果安全处理 |
| 到期与提前通知 | 提前通知延期；达到真实领域期限后 expired；测试调整专用夹具时间，不等待 24 小时 |
| 删除共享对象的一个与最后一个引用 | 单引用删除后仍可下载；最后引用经授权 GC 后 PG deleted 且 MinIO 字节不存在 |
| 三个真实分片按 3、1、2 上传 | 缺片 complete 409，恢复快照包含已确认分片；重复 confirm 200、冲突 ETag 409、complete 重放 202；合并下载 SHA256 一致 |
| 同 owner 重复内容、其他 owner 声明同 hash | 前者 409；后者必须实际上传，校验成功后复用 canonical；不将 hash 声明当作内容拥有证明 |
| fulltext、semantic、hybrid、name | 真实 ES/Qdrant 正常使用，中文短语与单字召回符合断言，文件名拼写容错可用 |
| ask 单文件、多文件、全库范围 | 有答案与版本化引用，引用不越出 scope；无效或空范围返回 409/400 |
| ES/Qdrant 单独故障、同时故障、超时与恢复 | 200 且保留可用证据；对应分支回退 PG；恢复后重新使用外部后端并清除 degraded |
| 无 token、错误 token、其他 owner | 401 或范围拒绝，不能读其他用户文件或证据 |
| 查询期间 rename/move/delete | 在读取屏障处提交修改，原请求返回 409 ai.scope_changed；后续请求读取最新元数据或拒绝已删除文件 |
| AI summary 单独重建 | 用户作业 succeeded，result_version_refs 对应实际发布的 summary 版本 |

## 白盒对账与故障边界

新增可选 `TEST_CHAIN_WHITEBOX=true`，在真实角色与独立夹具退出前对账。核验 `reference_count` 等于有效 file_entries 数、occupied_bytes 等于有效逻辑文件大小加上传预留、终态会话没有剩余预留、completed 会话的 entry/object/owner 身份一致、有效文件不能引用非 ready 对象。

AI 检查 active source/text/summary/vector 的依赖版本、父子片及向量数量、projection complete 的 expected/applied 数量。v3 另检查控制 results/executions/task_deliveries/task_continuations 没有新数据、AI 不产生通用 artifact，已完成 AI 任务各有一条稳定回执。

外部检查直接读取真实 ES 文档与 Qdrant points，逐项比对 PG active 子片的内容、父片/版本身份、向量维度与余弦一致性（1024 维，cosine ≥ 0.999999）。退休版本在宽限期内可保留物理记录，因此额外物理记录必须能映射到保留的规范化 PG 数据，不能简单以物理总数等于 active 数判断一致性。

AI PG 已有及新增连接切断时，业务上传仍完成；恢复后原任务继续并发布能力。业务 PG 切断时，已调度的摘要仍能在 AI PG 发布一条回执，业务作业终结单独重试并恢复。1→2→1 副本验证包括 etcd 实际轮询两个 query 实例、切断并恢复某个 query 实例 TCP 连接、消费者退出期间完成六个重建作业。

## 已识别的问题和测试修正

1. **秒传功能边界仍与简略功能描述不一致。** `FindOwnedWith` 只返回当前 owner 已有有效 entry 的对象；`drive.CreateFile` 与迁移 7 又禁止同 owner 重复引用同 object。因此正常新请求不能通过该分支创建新的秒传条目；同 owner 返回 `upload.file_exists`，其他 owner 要求 multipart。这是既有行为，源码事实文档与 11-19 上传验证已记录。本轮初始补测错误地把跨 owner 秒传设为预期，已改成验证持有证明与校验后去重；没有放宽权限或更改对象唯一规则，也没有将“新增条目秒传成功”记为通过。
2. **原专用 MQ 容器不能验收重启持久性。** `bx-coord-v2-test-mq` 的 `/var/lib/rabbitmq` 是 256 MiB tmpfs，重启后新增账号、vhost 与队列消失。首次重启测试因此失败，并影响紧随的部分 race 测试。已恢复本轮连接所需测试账号/vhost；已消失的旧测试队列不能视为恢复。本轮另建挂载独立持久卷的 `bx-backend-chain-mq-20261119`，用于复验 confirmed 消息跨重启保留。失败原始证据与环境修复后结果分别保存，不能删除失败记录后宣称首次全绿。
3. 故障注入中的 quarantine、预算耗尽、预期权限拒绝、数据库断连日志，需要结合对应测试和恢复结果判读；单独的 ERROR 行不等于未恢复的业务异常。

## 结果和证据

本轮机器可读汇总为 `artifacts/backend-chain-20261119/validation-summary.json` 和 `results.json`，串行执行入口为同目录 `run.py`；日志经过密钥脱敏，不保存测试 DSN 或模型密钥。`evidence-extract.json` 提取关键不变量与 skip，`sha256.json` 记录文件校验值。

| 检查 | 最新实际结果 | 原始证据 |
|---|---|---|
| 核心 HTTP 黑盒 v2 / v3 | 各 1 个主链路通过 | `TestAsyncCoreRealDependencies.jsonl` / `TestAsyncCoreDirectRealDependencies.jsonl` |
| AI HTTP 黑盒 v2 / v3 | 各 17 项通过（含父用例） | `TestAISplitRealDependencies.jsonl` / `TestAISplitDirectRealDependencies.jsonl` |
| 后端全包回归，E2E 已单独执行 | 42 包通过；1027 项通过、10 skip、0 fail；其中 577 个顶层测试通过；39 个包没有测试文件 | `whitebox-full.jsonl` |
| 扩展核心链路与白盒 v2 / v3 | 各 2 项通过，含三分片、下载 SHA、分享生命周期、跨 owner 校验后去重和领域对账 | `audit-TestAsyncCoreRealDependencies.jsonl` / `audit-TestAsyncCoreDirectRealDependencies.jsonl` |
| AI PG / 业务 PG 断连与恢复、在线矩阵、白盒 | 18 项通过；39 个已完成 AI 任务各有一条回执，7 个 active 子片及向量逐项一致 | `audit-TestAISplitDirectFailureIsolationRealDependencies.jsonl` |
| 1→2→1 副本、六个重建作业、在线矩阵、白盒 | 18 项通过；94 个已完成 AI 任务回执一致；10 个 active 子片/向量与外部一致；两端各 18 条物理记录均能对应保留 PG 数据 | `audit-TestAISplitDirectScaleRealDependencies.jsonl` |
| 真实 DashScope 小样本 | 1 项通过：图片事实 BX7319 → source → 父子片 → 实际 embedding / summary → PG fulltext / semantic → 实际 Ask 与版本化引用；不含真实 reranker 评测 | `real-model.jsonl` / `real-model-settings.json` |
| MQ 持久化重启与短时稳定性 | 9 项通过；重启后 20 条 confirmed 消息保留；5 秒内 240 confirmed、240 unique consumed、0 duplicate，覆盖 6 个队列 | `mq-recovery.jsonl` |
| race：asynccontrol / asyncjobs / aisystem / upload / sharing | 5 包通过；374 项通过、4 skip、0 fail，含真实进程 SIGKILL 和领域事务恢复 | `whitebox-race.jsonl` |
| Python parser | 17 项通过 | `parser.log` |
| go vet -p=1 ./... | exit 0 | `vet.log` |
| gofmt / git diff --check | 通过 | `final-checks.json` |

计数不跨轮次相加，避免把重复运行算成额外覆盖。第一次全包回归命令为 `go test -json -p=1 -count=1 -timeout=20m '-skip=^Test(AsyncCore|AISplit)' ./...`；排除的六个 bootstrap E2E 入口全部在表中的黑盒或故障专项独立执行。全包回归之后只增加可选测试辅助代码，再执行了扩展 E2E、AI 故障及索引对账、上述五包 race 和全仓 vet；没有把此前的全包结果写成最后再次整仓重跑。

全包 skip 为独立读写性能微基准、显式真实模型开关、MQ restart/stability 开关和只由父测试调用的子进程入口。后续专项补跑了 v3 真实模型、MQ restart/stability；v2 真实模型和读写性能对照仍未执行。race 的 4 个 skip 为两项性能对照及两个真实模型可选入口；v3 真实模型的独立通过不能改写原始 skip。子进程 helper 的直接 skip 不表示父测试没有执行其崩溃恢复窗口。

`initial-*.jsonl` 保存最早的错误秒传预期，`before-final-*.jsonl` 与 `before-final-results.json` 保存最终修正前的测试预期/MQ 环境失败。首次受 MQ 环境影响的 race 为 360 pass、4 skip、14 fail；恢复账号/vhost 后完整复跑上述五包，最新为 374 pass、4 skip、0 fail。业务实现没有为通过这些测试而放宽规则。

全量和专项中的 ERROR 日志主要来自明确测试：同语义身份不同输入隔离、回执冲突隔离、终态到期矛盾隔离和恢复预算耗尽；相应测试通过，故障解除后的恢复由独立断言核验。没有发现本轮 live 夹具中的配额、引用、会话、active 版本、回执或索引物理记录不一致。

复跑时先在调用环境导出专用依赖的 `TEST_*` 变量，再在仓库根目录使用 `python3 artifacts/backend-chain-20261119/run.py <phase>`。阶段为 `e2e`、`whitebox`、`faults`、`real-model`、`mq-recovery`、`parser`、`race`、`vet`；其中 `e2e` 校验专用来源库后建立本轮数据库，`faults` 设置 `TEST_CHAIN_WHITEBOX=true`。脚本不会替用户修改 `deploy/.env`。

## 运行状态与未覆盖范围

仅新增测试辅助代码、文档及 `.gitignore` 文档白名单，没有修改业务实现、生产配置、协议或执行 contract 归档。保留本轮独立测试数据库、证据与 MQ 持久卷；本轮新增 `bx-backend-chain-mq-20261119` 和 `bx-backend-chain-redis-20261119` 仍运行以便复验。测试 bootstrap 的应用角色已经退出。既有业务数据、卷、缓存及用户配置未做清理；原临时 MQ 重启造成的测试队列丢失单独见上文，不能将其写成数据已保留。

这是单机、有限样本的功能与恢复验证。真实模型样本不能推广为通用问答质量或 OCR 能力认证；同实例三库不是三个 PG 实例的资源隔离；副本变化与 TCP 故障不是跨主机 HA、PG 主从切换或容量验收。未执行存量协议切换、生产镜像部署、大规模历史迁移或默认 24 小时 GC 等待。
