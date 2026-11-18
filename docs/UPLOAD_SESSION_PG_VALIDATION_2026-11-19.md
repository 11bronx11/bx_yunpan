# 上传会话业务 PG 专项验证（2026-11-19）

本轮验证窗口／批量上传改造后的业务库边界，新增 18 个主测试，并修复 3 处业务问题。前端源码、AI、缩略图和其他派生消费者没有参与修改或运行。本报告描述当前工作区的验证，不代表已经部署。

## 验证环境和边界

- 新建独立 PostgreSQL 17 容器 `bx-upload-session-pg-20261119`，数据库 `upload_session_validation`，与已有开发、业务和历史测试容器分开。执行业务迁移 000001–000036 的 SQL；集成夹具再在库内建立独立迁移 schema。
- 使用 Go 1.25.13。HTTP 测试直接调用真实 Gin 路由，发送前端相同的字段、路径、`view=delta` 和 `Idempotency-Key`；身份由测试中间件注入，不验证登录鉴权系统。
- 真实执行会话、分片、操作意图、outbox、配额、对象／文件条目和 receipt 的 PG 事务。预签名使用固定测试凭据和离线签名域名，不向对象存储上传字节。
- 调用真实 planner 和 committer 的业务事务入口，注入类型化计算结果；不启动控制 PG、RabbitMQ、MinIO、浏览器或应用角色。
- “响应丢失”通过丢弃已执行请求的响应并重放相同请求模拟；“弱网取消”通过真实 PG 行锁等待期间取消请求 context 验证。没有执行实际限速、丢包或浏览器传输测试。

## 已修复的问题

1. **过期会话被取消改写终态。** 原来 DELETE 会把 `expired` 改为 `aborted` 并增加版本。现在过期、失败、已取消会话的 DELETE 都只做幂等返回，保留原结论。修改前的测试实测为 `expired/v3 → aborted/v4`。
2. **兜底过期扫描缺少清理意图。** 原来只对已知 upload ID 登记 multipart.abort；未知创建结果缺少 sweep，暂存对象也缺少显式清理。现在 `expireOne` 与正常异步过期提交复用 `failAsync`，在同一事务中释放预留、登记 `upload.expired`、按需中止已知 multipart，并登记 sweep／staging.delete。这里只验证清理意图入库，不宣称物理字节已经删除。
3. **Complete 将数据库故障误报为状态冲突。** 原来 UPDATE 的 SQL 错误会被统一转换成 409。触发器注入 UPDATE 失败时已复现 `409 upload.conflict`。现在保留实际数据库错误，由 handler 返回 500；事务整体回滚，故障解除后原请求可正常进入 completing。

没有新增数据库迁移或修改请求格式。兜底过期现在与正常过期路径一致地返回 `error_code=upload.expired`，并产生完整的清理意图，这是本轮的实际行为变化。

还修正了旧测试的时间精度误报：首次 delta 的 Go 时间可能含纳秒，PG 持久值为微秒，不能靠 Round 推定数据库值；现在读取已持久化时间再验证冻结重放没有改写它。并给上传并发夹具补齐 fake 对象存储，使旧清理兼容用例能处理新增的 staging.delete。

## 已验证场景

| 场景 | 实际断言 |
| --- | --- |
| 创建结果丢失 | 原幂等键／参数找回同一会话；仅一个创建意图、一次配额预留 |
| 批量签名 | 20 片成功、21 片拒绝；空列表／重复片号／越界拒绝；URL 含正确会话 uploadId、片号和 `/storage` 路径 |
| 批量确认 | 100 项成功、101 项拒绝；相同重复项去重；冲突重复项、非法大小、尾片、片号、ETag 和 checksum 超长整批拒绝 |
| delta／旧响应 | delta 只返回本批 ACK；不带 view 返回完整累计确认列表 |
| SQL 批量行为 | 100 片 delta 只有一次批量 INSERT 和一次本批片号查询；预签名不读历史确认片 |
| 乱序／重叠并发 | 12 个重叠批次并发确认，最终片号唯一、不丢失；两个设备用不同 ETag 争同一片，一个成功、一个 409 |
| 冲突批次原子性 | 已有片冲突时，同批另一个新片也不提交；已有 ETag 和 completed_at 不变 |
| 暂停／断点恢复 | 停发后 GET 返回完整、排序后的确认片；恢复只补缺片；缺片时 Complete 拒绝 |
| 确认 ACK 丢失 | 原 ETag 重放四次仍幂等；确认时间和预留不重复变化 |
| 确认故障耗尽 | 注入分片 INSERT 故障并请求四次，整批回滚；会话仍可恢复，解除故障后原批成功 |
| context 取消 | 确认真正在 PG 等待会话锁时取消；释放锁后没有迟到写入，之后可正常重试 |
| 状态竞争 | 由 PG 阻塞证据确定锁顺序，覆盖 confirm→complete、complete→confirm、abort→confirm、confirm→abort、abort→complete、complete→abort、expiry→confirm、complete→expiry 八种交错 |
| Complete 响应丢失 | GET 看到已持久化 completing；重复 Complete 只形成一个合并意图；此后取消返回冲突 |
| 合并输入冻结 | planner 读取的片号／ETag 与确认事实一致；completing 后冲突确认不能改写输入；一致重放仍可成功 |
| 合并结果重复 | 八个业务提交事务竞争相同结果，只有一个 receipt 和一个校验事件 |
| 发布失败与重放 | receipt 插入故障使对象、文件条目、会话完成和配额结算一起回滚；重试成功；相同结果再提交四次不重复引用，也不登记删除 active 对象的操作 |
| 错误／迟到结果 | 错 owner、错误复制 bucket／key／空世代拒绝；同语义键不同结果被 receipt 拒绝；失败／过期后的创建成功只登记孤立 multipart 中止，不复活会话 |
| 内容校验失败 | SHA-256 不符转 failed，释放预留，不发布条目 |
| 异步耗尽业务收敛 | 注入创建／合并的耗尽结果，failed、稳定错误码和释放配额只生效一次；这里未运行控制库的尝试次数计数器 |
| 内容竞争转变 | 两个已启动会话争相同内容：第一个发布；第二个校验时发现 ready 对象和已存在的拥有关系，按实际前提失败并释放第二份预留 |
| 同内容／秒传入口 | 已持有同内容的新请求返回当前契约的 `upload.file_exists`；其他用户只声明相同 hash 仍进入 multipart，成功校验后复用 ready 对象，不重复复制 |
| 终态和显式重试 | failed／aborted／expired 不接受签名、确认或 Complete；原创建键不重开终态；新键可创建新会话；终态从 active 列表移除 |
| 过期回收 | 未到期结果重新定时且不写 receipt；到期释放一次预留并登记清理；未知创建结果迟到仍被中止；已接纳的合并不被过期扫描打断 |
| owner 隔离 | GET、签名、确认、Complete、DELETE 均不能读取或修改其他用户的会话 |

当前代码的 `mode=instant` 创建分支仍存在，但 `FindOwnedWith` 要求已有有效的拥有条目，而 `drive.CreateFile` 又禁止同一用户重复持有同对象；正常重复上传因此按契约返回 409。本轮验证了该实际行为，没有把不可达的“新增条目秒传成功”写成通过，也没有调整去重或跨用户权限策略。

## 检查与证据

本轮执行的测试／检查结果保存在 `artifacts/upload-session-pg-20261119/`；以 `summary.json` 和 `checks.json` 为最终汇总。

| 检查 | 最终实测 |
| --- | --- |
| 新增专项测试 | 18 个主测试全部通过，另含参数、状态及竞争顺序子场景 |
| upload 包 | 42 个主测试通过；2 个真实 MinIO 用例跳过 |
| identity／storageops／ownerepoch | 分别 7／8／1 个主测试通过 |
| asyncjobs 业务库专项 | 7 个主测试通过；没有启用其他 asyncjobs 控制库测试 |
| 合计（不重复计入 race） | 65 个主测试通过，2 个跳过 |
| race | 19 个主测试通过，其中含 18 个新增主测试及原有会话响应测试 |
| vet／build／格式 | 上述相关包 vet、全部 `cmd/...` 构建、gofmt 与 `git diff --check` 通过 |

```bash
# TEST_POSTGRES_DSN 必须指向已迁移的专用测试库。
GOTOOLCHAIN=go1.25.13 go test -p=1 ./internal/upload ./internal/identity ./internal/storageops ./internal/ownerepoch -count=1
GOTOOLCHAIN=go1.25.13 go test -p=1 ./internal/asyncjobs -run '^TestUploadBusiness' -count=1
GOTOOLCHAIN=go1.25.13 go test -race -p=1 ./internal/upload ./internal/asyncjobs -run '^(TestSession|TestUploadBusiness)' -count=1
GOTOOLCHAIN=go1.25.13 go vet -p=1 ./internal/upload ./internal/asyncjobs ./internal/identity ./internal/storageops ./internal/ownerepoch
GOTOOLCHAIN=go1.25.13 go build -p=1 ./cmd/...
```

命令在 `backend/` 执行。日志中的 MinIO 专用集成用例因未设置 `TEST_MINIO_ENDPOINT` 跳过，单列为未执行。没有运行后端全包测试、前端测试或 AI 测试。

主要证据：`upload-final.jsonl`、`regression.jsonl`、`business.jsonl`、`race.jsonl`、`vet.log`、`build.log`；修改前失败证据为 `fences-before-fix.jsonl`、`complete-error-before-fix.jsonl`。配置凭据只在本地私有文件中，不作为报告附件。

## 运行状态和未验证范围

仅修改业务代码、测试和本报告；没有部署或重启业务栈。测试结束后停止本轮新建 PG 容器，保留其测试数据库和卷；夹具按原有约定删除自身临时 schema。原有工作区中的 AI／gRPC 等修改保持原样。

PUT 单文件 4／全局 8、未确认窗口 16／32、自动退避与重签、真正的限速／丢包、前端暂停 drain 和调度公平性属于客户端或传输层，本轮不作验证结论。服务端本轮验证的限制是签名 20／确认 100 的请求边界和 PG 状态屏障。

RabbitMQ 投递、独立控制 PG 的执行预算／失租／重派发、实际 MinIO 合并与下载哈希、物理过期删除、跨进程崩溃恢复、容量和跨主机高可用均未执行。本轮“正常链路完成”指业务 PG 从创建到文件／对象／引用／配额及 receipt 的完整状态提交，不等同于真实字节传输 E2E。
