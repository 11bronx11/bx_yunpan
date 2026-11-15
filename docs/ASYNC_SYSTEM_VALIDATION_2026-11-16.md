# BX YunPan 异步系统测试执行报告

日期：2026-11-16（北京时间；原始日志使用 2026-11-15 UTC）。

按用户要求，把测试计划作为参考，执行可在现有主机上隔离完成的测试，省略成本高或缺少环境的项目。**选定的本地验证已完成，发现并修复 3 个产品缺陷；完整计划的全部矩阵未执行。未部署到现有业务栈。**

参考：[当前实现说明](ASYNC_SYSTEM_IMPLEMENTATION_2026-11-16.md)、[测试计划](ASYNC_SYSTEM_TEST_PLAN_2026-11-16.md)。计划文件中的“本次只写计划”描述上一轮，不是本轮执行限制。

## 1. 被测版本与证据

- HEAD：`fedf51f945f1b4df04bcef0a9dfc52f6526d0b6a`。异步实现仍包含大量原有工作区修改/未跟踪文件，不能仅凭 HEAD 重建被测版本。本轮没有提交或重置这些修改。
- Run ID：`20261115T110102Z`，证据根目录：`artifacts/async-validation/20261115T110102Z/`。以下证据路径均相对该目录。
- 最终后端结果使用 `confirmed/`，最终独立进程和浏览器使用 `process-confirmed/`。早期 `verified/`、`process-final/` 等目录保留为修复前后的历史，不能按名字推断最终结果。
- `source-snapshot.json` 保存最终工作区文件摘要；`confirmed/*-manifest.json` 保存命令、退出码、耗时、必要用例、跳过项及执行时文件摘要；`migration-hashes.json` 保存 72 个业务/控制迁移文件摘要。
- `process-confirmed/binaries.json` 和 `frontend-hashes.json` 标识实际运行的二进制和 Web 生产构建。最终源码与回归快照的 Go/SQL 摘要一致性见 `final-checks.json`。
- 可移交证据包：`evidence-sanitized.tar.gz`；包内 `SHA256SUMS.txt` 校验各成员，包外同名 `.sha256` 校验压缩包。连接配置、登录夹具、浏览器 trace、二进制和依赖目录不进入证据包；私有原件仍保留本地。

## 2. 实际执行结果

| 执行项 | 实际结果 | 证据 |
|---|---|---|
| Go 全入口构建、全量 vet | 均退出 0 | `confirmed/build.log`、`confirmed/vet.log` |
| 后端全量回归 | **403 顶层 pass、0 fail、5 skip**；含子测试 **612 pass、6 skip** | `confirmed/regression.jsonl`、`confirmed/regression-manifest.json` |
| 核心与领域 race | **127 顶层 pass、0 fail、0 skip**；含子测试 **223 pass** | `confirmed/race.jsonl`、`confirmed/race-manifest.json` |
| 旧 taskqueue 传输 race | 指定传输用例重复 3 次通过 | `transport-race.log` |
| 前端 lint、生产 build | 均退出 0 | `web-lint.log`、`web-build.log` |
| 前端全量 Jest | **20 suites / 121 tests 通过** | `web-test-final.log` |
| Python parser | **17 tests 通过** | `parser.log` |
| 独立二进制 + Nginx | **14 项通过** | `process-confirmed/cases.json`、`summary.json` |
| Chromium 浏览器 | **3 轮独立 context 通过**，两轮 1440×1000、一轮 390×844 | `process-confirmed/browser-results.json`、截图 |
| Backoff 模糊测试 | 真实执行 30s，73,126 次输入 | `fuzz-backoff-retry.log` |
| Digest 模糊测试 | 真实执行 30s，57,029 次输入 | `fuzz-digest-retry.log` |
| 退避/并发领取重复 | 退避 100 次；32 goroutine 领取用例重复 20 次 | `backoff-fixed-100.log`、`claims-20.log` |
| 配置与静态检查 | 两模板 230 个唯一键一致；全 profile Compose 渲染、shell/Python/Node 语法、Go 格式、diff 检查通过 | `compose-check.json`、`final-checks.json` |

回归和 race 均启用真实业务/控制 PG、RabbitMQ、MinIO；真实 Redis 故障回退已实际运行。全量回归另启用真实 ES/Qdrant 和真实中文 parser 测试，不是依赖未设导致 skip。

最终后端全量命令为 `go test -p=1 ./... -count=1 -json -timeout=5m -skip <精确退役名称正则>`。四个精确排除项为 `TestAsyncUploadAndGCWithTwoMaintenanceScanners`、`TestDispatcherRealRabbitCrashBeforeACK`、`TestRealRabbitLostDeliveryReplaysOriginalTask`、`TestRealRabbitLostDeliveriesAcrossFiveQueues`；它们属于退役执行链，不恢复旧启动方式。其他旧兼容用例继续运行，其通过只能证明对应兼容语义。

最终 5 个顶层 skip 是 4 个仅供父测试启动的子进程 helper，以及主动不执行的 `TestRabbitMQStability`。第 6 个含子测试 skip 是旧传输层的 broker 重启子项。父级强杀/redelivery 测试实际通过；broker 重启未伪装成通过。必要用例缺失、意外 skip 和失败均由 runner 判失败。

前端最终命令为 `CI=true npm test -- --runInBand --watchAll=false --testTimeout=30000`；Shares 用例自身明确设置的 15s 超时没有修改。首轮超时失败仍保留，见第 6 节。

## 3. 独立进程、浏览器与对账

使用专用新数据库、新 bucket、新 MQ prefix，运行双 API、双 async-control、双 committer，以及 upload/object/media/gc 各一个计算进程。二进制由当前源码串行构建；Web 使用本轮生产构建。Nginx 轮询代理两台 API，并提供同源 `/storage` 到专用 MinIO 的链路。用户输入走公开 HTTP，没有用调用内部服务函数代替进程 E2E。

这是 **E1 单机多进程验证，补充了部分 E2 所需的浏览器/代理证据**。没有启动完整隔离 Compose 项目，也没有把每种计算角色扩到两个副本。测试从受保护的 `TEST_*` 配置生成角色环境，没有改 `deploy/.env`；因此不认定 `up.sh` 全部署切换路径已验收。

关键实际结果：

1. 空业务/控制库迁移及重复 up，最终分别 `34|dirty=false`、`2|dirty=false`；API 配置的控制 PG/MQ 不可用时仍完成 readiness 与事实准入。
2. 单分片及 **20MiB+333B 三分片**上传，经 `/storage` PUT、逆序确认、完成，再经代理下载 SHA-256 一致。
3. 双 committer 各持有 16 个结果分片。在持久 computed 屏障后强杀一台，另一台接管 32 分片并完成业务提交；generation/attempt 不增加，receipt 唯一。重启后重新均衡为 16/16。
4. 强杀 Relay 租约持有者，备用进程接管并受理后续事件，再重启原进程。
5. initializing 取消及晚到工作、SHA 拒绝、错误 ETag 导致真实 MinIO 长错误后有限耗尽，均得到明确终态；错误会话预留释放。
6. 分享导入使引用从 1 到 2；删除原文件后收件人仍可下载；最后引用删除后经授权回收。
7. 仅做并发 2/4、合计 **12 次小文件上传**功能样本。不是负载曲线、饱和点或容量测试。
8. 浏览器三轮均实际登录、新建目录、选文件上传、观察 `/storage` PUT 200、等待 completed、下载校验 SHA、刷新及打开第二标签页。移动视口通过菜单下载。全部 `page_errors=[]`。Playwright 1.61.1，Chromium 149.0.7827.0。

收尾停止生产并等待立即可执行工作排空。业务 **同一个 repeatable-read SQL 快照**得到 `quota_mismatches=0`、`reference_mismatches=0`、`bad_completed_sessions=0`。跨库对账检查 **149 个任务、123 条 receipt，差异 0**；其余 26 个任务为未来到期/安全窗口工作（22 个 upload.expire、4 个 multipart.sweep），没有强制提前清理。控制库无 needs_attention/quarantined/exhausted/convergence_pending，quarantine 数量 0；故意制造的失败和拒绝任务保留其真实终态。

证据：`business-reconcile.txt`、`quiescent-cross-db.json`、`control-summary.json`、`events.jsonl`，均在 `process-confirmed/`。这次只做收尾快照，不宣称压力期间持续采样的全部不变量都已证明。

## 4. 本轮修复

| 缺陷 | 触发与影响 | 修改与回归 |
|---|---|---|
| Backoff 极值 | 正 duration 在极值转换或 NaN jitter 下可能得到 0/负数，形成热重试风险 | `asynccontrol/model.go` 修复浮点调整、溢出与正下界；新增边界和 fuzz 测试。修复前失败、100 次回归和 30s fuzz 均留证 |
| 上传失败码过长 | 真实 MinIO 诊断直接写入 `upload_sessions.error_code varchar(128)`，触发 SQL 22001，失败事务及配额释放回滚 | `upload/async_commit.go` 保留稳定领域码，将任意远端诊断映射为 `upload.async_exhausted`；完整诊断仍在控制结果。定向用例验证一次释放，真实错误 ETag 进程 E2E 通过 |
| 后台入库缓存不可见 | 浏览器上传 completed 后刷新仍读到旧目录；目录缓存及 cursor 只取 API epoch，漏取 async epoch | `drive/service.go` 统一调用 `ownerepoch.Current`，`ownerepoch/epoch.go` 用同一 SQL 快照组合两个计数器。新增 `TestCachedPagesObserveBackgroundContentEpoch` 检查旧缓存和旧游标；修复前定向失败，修复后全量/race/三轮浏览器通过 |

另外修复两个测试隔离问题：RabbitMQ transport 子用例分别使用独立 vhost，避免 requeue 残留污染后一个子用例；backlog metrics 测试改用独立迁移 schema，不再统计专用测试库 public 中其他历史行。没有通过删除历史行获得绿色结果。

新增可复用入口位于 `deploy/tests/`：`async-check.py`、`async-process.py`、`async-browser.cjs` 及 README。后端/进程套件使用同一资源文件锁，必要用例缺失时拒绝通过，保留原始退出码和每轮现场。`.gitignore` 为本报告增加 docs 白名单。原有大量异步实现、启动脚本等修改不是本轮重新实现，未混称本轮新增功能。

## 5. 计划覆盖与明确省略

`plan-cases.json` 登记 **147 个条目（含 14 个不变量和 8 个负载条目）**。其中 101 个条目有实际通过的有限子集，全部按具体测试名称和证据路径关联；不把子集通过写成整行矩阵通过。严格整行状态为 PASS 3、NOT_RUN 137、BLOCKED 6、KNOWN_DEFECT 1。这不是“通过率”：大多数 NOT_RUN 条目下面仍有已执行子集。整行 PASS 为 CT-04、BE-01、OP-06。

| 计划范围 | 本轮执行边界与省略理由 |
|---|---|
| 契约、Relay、预算、批提交、维护、导入 | 现有真实依赖集成重跑，加边界/fuzz/32 并发领取；没有新增独立状态机 oracle、100 seed 模型或每个故障点 20 次进程重复 |
| 上传、引用、GC、权限、缓存 | 核心冒烟、领域回归、进程/浏览器选定路径及收尾对账；没有把每种状态、API 交错、旧调用晚到的笛卡尔积全部执行 |
| 实际 AI/parser/search | 真实 Go→Python 中文 parser、fake embedding、真实 ES/Qdrant adapter、中文检索、重建/退役/墓碑回归；没有付费真实模型质量验证，也没有独立 etcd/gRPC parser/aisvc 全拓扑故障演练 |
| 网络/数据库/MQ/MinIO 故障 | 执行选定 SIGKILL、computed/receipt 恢复、真实 Redis pause 回退和 MinIO 错误收敛；未做定向单向分区、整库重启、磁盘/OOM/FD 耗尽、时钟偏移及完整黑洞/丢 confirm 矩阵 |
| 多主机与基础设施 HA | DR-01～DR-06 为 BLOCKED：没有对应独立恢复副本/跨主机/集群环境；同 PG 实例两数据库不能证明资源隔离，单节点 broker 不能证明 quorum 集群容灾 |
| 双库 PITR/恢复和切换回退 | 不在现有数据上做破坏性恢复。未执行双库不同时间点恢复、对象版本错位、真实旧→新全部署切换及有新写入后的回退 |
| 规模和容量 | 未造 10万/100万历史任务，未做大表迁移锁/回填演练，未做 E4 生产近似短时压测。12 次并发样本不能外推容量、公平或资源上限 |
| 浏览器扩展 | Chromium 桌面/移动通过；Firefox/WebKit、断网、JWT/代理故障和所有分享权限交错未运行 |
| 长压测/浸泡/自然生命周期 | 按计划已取消，不作为尚待完成的验收阻塞项。生命周期仅有限配置/加速边界证据 |

**BE-07 既有 Create 响应丢失后取消竞态仍为 KNOWN_DEFECT**。本轮没有加该屏障重现，也没有修复，不能由其他取消/浏览器通过项推导它已解决。

## 6. 首次失败与复测记录

详见 `first-failures.json`，修复前日志未覆盖：

- 首轮 9 个 RabbitMQ 管理用例 401：测试账户权限不足；改用本轮独立 broker，没有修改原 broker 权限。
- transport 子测试 requeue 串扰、metrics 历史行串扰：修复夹具后全量通过。
- 早期故障套件与 race 并行，SIGSTOP 保留事务影响同 PG 实例全局 xmin，即使数据库不同也会相互阻塞。改为资源锁串行，并调整选定屏障；**没有据此认定所有 SIGSTOP/残留事务矩阵已完成**。
- 首轮 Jest 12 个 timeout，随后单独复测仍有 1 个 Shares timeout；最终串行全量使用 30s 默认超时通过，既有 Shares 15s 设置不变。
- 首轮 fuzz 在冷编译期间超过 120s 外层限制，没有得到 fuzz 结论；增加有界启动时间后，两个目标分别真实 fuzz 30s 通过。
- 新进程 harness 的关键字冲突、分片大小假设和 presign 数组顺序假设已修复；实际按 part_number 配对。浏览器中文按钮 locator 因图标/字符间距匹配失败，改正后继续执行，没有改前端代码。
- `process-final/` 浏览器真正完成上传后目录不可见：定位并修复组合 epoch 缺陷，保留截图/trace。最终 `process-confirmed/` 从新数据库和新二进制重新执行全部进程及浏览器流程后通过。

## 7. 运行状态与保留资源

未运行业务栈部署、停机或清理命令，未改 `deploy/.env`。原有 26 个容器最终 ID 与测试前完全一致，仍在运行，当前 StartedAt 全早于本轮开始。初始盘点没有记录 RestartCount，因此不作历史 counter 差值断言；可核验证据见 `containers-rechecked.json`。

本轮独立进程及 Nginx 全部停止，专用 Redis `bx-async-validation-redis-20261115t110102z`、RabbitMQ `bx-async-validation-rabbit-20261115t110102z` 已停止但保留容器和卷。原专用测试 PG/MinIO、ES/Qdrant 和原业务服务继续保留。

保留清单在 `resource-retention.json`：7 次进程尝试共 **14 个数据库、7 个 bucket 及相应 MQ prefix**。后缀为 `09882de650`、`005a1e565d`、`f40804c608`、`0476731d0c`、`741764000d`、`a2068ef39d`、`c7e7f4e6eb`；最终成功现场是最后一组。首轮 harness 曾在写 residual 文件时失败，资源仍由其 inventory 登记，最终另行核实无遗留进程。

测试依赖虚拟环境、浏览器 runner/引擎、Web 构建、测试二进制、私有配置、下载样本和 trace 仍保留在本地。未删除数据库、bucket、队列、卷或既有缓存。清理需要以后按该清单另行审阅；本轮不以清理作为完成条件。

复跑方式见 `deploy/tests/README.md`。本轮报告证明的是上述配置下的有限单机验证，不是生产部署、跨主机 HA 或容量认证。
