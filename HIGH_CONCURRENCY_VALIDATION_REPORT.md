# 高并发专项：隔离验证与修复报告

- 日期：2026-09-11
- 阶段：P1 补强、真实依赖故障窗口、观测能力；不是完整 staging 发布或容量验收
- 主要证据：[20260911T071039Z-1206723](artifacts/high-concurrency/20260911T071039Z-1206723/)

## 1. 本轮结果

已修复 Outbox 旧租约回退覆盖新 claim、跨副本业务缓存陈旧、Redis 缓存预算未落实到网络 I/O、上传单任务重试不收敛 succeeded operation 四个问题。补齐缓存、持久任务积压、租约接管和数据库连接池指标，以及三条初始告警规则。

| 验证 | 实际结果 | 证据 |
| --- | --- | --- |
| 全量 Go 测试，真实 PostgreSQL/Redis/MinIO | 207 项通过，1 项跳过，47 个包；数量包含子测试 | `tests.jsonl`、`summary.json` |
| `go vet ./...` | 通过 | `vet.log` |
| cache/drive/outbox/dbmetrics/upload race | 56 项通过，5 个包，无跳过 | `race.jsonl` |
| 最后调整 Outbox 积压年龄后的专项 race | dbmetrics 通过，确认重试不重置原事件年龄 | `backlog-age-recheck.jsonl` |
| 迁移 22 -> 21 -> 22 | 全部成功，最终 `dirty=false` | `migration.log` |
| 两个独立 API 的真实 HTTP | 跨副本创建、重命名、删除可见；12 个并发读结果正确；指标可抓取 | `http-cache-smoke.log` |
| Prometheus 配置、规则语法 | 通过 | 已执行 `promtool check config`、`check rules` |
| 告警触发与恢复测试 | 通过 | `alerts.log` |

唯一跳过项是 `TestExternalContainers`，需要真实 Elasticsearch 和 Qdrant 地址。本轮没有把它计为通过。

## 2. 复现与修复

### Outbox 旧 ACK 清掉新租约

旧副本租约过期，新副本成功 claim 后，旧副本 ACK 返回 lease lost。但是原 `ackDelivery` 的异常回退只判断 `status='sending'`，会把新 claim 改回 pending 并清掉 token。真实 PostgreSQL 回归测试在修复前失败。

回退 UPDATE 现在同时匹配原 `lease_token`。测试确认旧 ACK/失败写入不能覆盖新 token，新副本随后仍能完成 ACK。真实 Redis 测试另外验证：入队已成功而数据库仍为 sending 时，过期接管使用原 TaskID，遇到队列去重后正确完成数据库 ACK。这里模拟的是崩溃留下的持久状态，没有声称执行了进程 kill 故障演练。

### 跨副本缓存陈旧

原删除操作只能清掉当前进程 L1，另一 API 的 L1 仍会返回移动前的文件。worker 使用无缓存的 drive service 创建文件时也无法清除 API 的 L1。真实 PostgreSQL 回归测试在修复前失败。

缓存 key 现在包含 PostgreSQL 中已提交的 `content_epoch`、`acl_epoch`，每次读都查询版本；业务事务中的版本推进使所有副本转向新 key。旧请求即使晚填缓存，也只会填到旧版本 key。移除了 `CreateFile` 事务内的 Redis 删除。文件详情在读缓存前仍查询 owner、删除状态、对象 ready 状态，缓存不能授权下载已失效对象。

代价是缓存命中仍有数据库版本/权限查询；尚未宣称减少了总连接压力。版本按 owner 粗粒度变化，频繁写入可能降低命中率。AI 状态等派生展示字段仍可能受缓存 TTL 影响。

### Redis 50ms 预算

原 Redis client 未启用 context socket deadline，且一次 Load 可能执行两次 GET 和一次 SET，各自重新计时。现在 Cache client 明确配置 context deadline、连接/读/写/池等待超时，禁用重试；Load 的 Redis 读取与回填共用一个 50ms 预算，并在 singleflight 内执行 Redis miss 查询。

在独立 Redis 执行 `CLIENT PAUSE 1000 ALL`，本轮全量测试实测 **50.868ms** 后回源成功。150ms 是测试允许的调度误差上限，不是配置预算。数据库自身查询时间不包含在这项 Redis I/O 预算内。测试还覆盖真实 L2 读取及 Redis client 完全不可用时回源。

### 上传 succeeded operation 的单任务恢复

真实 MinIO 完成 multipart 后，用 PostgreSQL trigger 故意拒绝会话进入 verifying，确认对象已经存在。原单任务 `ReconcileStorageOperation` 看到 succeeded 就返回，未推进会话，只能等待周期扫描。修复后 multipart create/complete 的单任务重试调用已有收敛逻辑。

测试确认重复恢复只发一个校验事件；重复 Verify 后只存在一条文件记录、一次逻辑配额占用、一个对象引用，canonical 对象内容完全一致；预热过的另一 drive 缓存能看到 worker 提交的新文件。另在请求阶段阻断 MinIO transport，确认 Create/Complete 不触发存储请求。此项是 service 层验证，未冒充完整上传 HTTP 故障测试。

## 3. 索引证据

独立数据库中生成 50 个 owner，upload、operation、delivery、object、task 各 50,000 行，file entry 49,900 行。执行 `ANALYZE` 后保持默认 planner 设置，未关闭顺序扫描。数据放在事务内，结束后回滚。

| 查询 | 实际索引选择 |
| --- | --- |
| active upload | `idx_upload_sessions_active` |
| 同目录 active filename | `idx_upload_sessions_name_active` |
| storage 实际混合 claim | 原 `idx_storage_operations_claim`；没有命中新 ready/lease 索引 |
| storage 单独 ready/expired 查询 | 分别命中新增 ready、lease 索引；只是诊断 SQL |
| Outbox 实际混合 claim | BitmapOr 合并新增 ready、lease 索引 |
| owner 过滤的文件名查询，批量插入后 | owner 活跃索引，然后过滤名称 |
| 同一文件名查询，GIN pending list 维护后 | `idx_file_entries_name_trgm`，仍保留 owner/deleted 过滤 |
| object GC / async recovery | 分别使用 GC / recovery 索引 |

文件名查询的 GIN 选择依赖 pending list 维护和数据分布，不能写成无条件命中。storage 实际查询仍选择旧索引也不等于全表扫描。以上执行时间只说明本次合成数据、缓存和虚拟机环境，不代表生产 QPS。

## 4. 指标与告警

- API：`yunpan_cache_lookups_total`、`source_loads_total`、`redis_errors_total`、`l1_evictions_total`、`shared_loads_total`，没有 owner/key 标签。
- Relay：`yunpan_outbox_backlog`、`oldest_age_seconds`、`backlog_scrape_success`、`lease_recoveries_total`。
- Maintenance：对应 `yunpan_storage_*` 持久 operation 指标。
- API、relay、object/media/maintenance/search worker：标准 `go_sql_*` 连接数、等待次数与等待时间。AI 专属角色的连接池指标仍待后续接入。

积压从 PostgreSQL 采集，每次 scrape 的查询超时为 2 秒；失败时输出 scrape_success=0 并省略积压样本，避免显示成零。已消失的积压标签从下一次 scrape 消失。Outbox 年龄从事件创建时计算，重试修改 available_at 不会掩盖长期滞留。

新增积压滞留、采集失败、缓存 Redis 错误三条告警。阈值是初始观测阈值，未经过容量标定。规则触发/恢复测试通过，但没有启动生产 Prometheus 或验证通知投递。

## 5. 重跑与环境保留

```bash
HC_GO_BIN=/home/bronx/.local/go/bin/go bash deploy/high-concurrency-check.sh
```

依赖 Docker、已有的三个依赖镜像及 Prometheus 镜像、Go 1.25、Node.js、curl。普通环境可省略 `HC_GO_BIN`；本机 Go 外层有输出包装器，指定实际二进制保留原始 JSON 测试日志。

脚本使用独立 `bx-yunpan-hc` Compose 项目，端口默认为 15432、16379、19000；可用 `HC_POSTGRES_PORT`、`HC_REDIS_PORT`、`HC_MINIO_PORT` 更换。每次运行创建新的测试数据库，并给 race 阶段使用另一数据库。两个 API 进程使用动态空闲端口，退出时自动停止。依赖容器由脚本保留，可用 `docker compose -f deploy/compose.high-concurrency.yaml stop` 停止，数据仍保留。

首次测试编排遇到两个夹具问题：同一事务连接被两个 collector 并发使用；全量测试残留事件影响后续 relay 全局 claim。分别通过拆分采集 registry、使用新 race 数据库及调整测试包执行顺序修复。失败记录保留在此前两个 artifact 目录中。测试数据库必须独立，不能把 `TEST_POSTGRES_DSN` 指向开发或生产业务库。

最终整套脚本运行目录为 `20260911T071039Z-1206723`，输出 `ISOLATED_CHECK=PASS`；之后仅调整了积压年龄查询，并通过独立 dbmetrics race 回归。本轮结束前已停止所有临时 API 和隔离依赖容器，没有删除测试数据库、对象或 volume。原开发服务健康检查通过，数据库仍为 **21 / dirty=false**，未修改 `deploy/.env`，未发布现有应用容器。

## 6. 下一阶段

1. 目录 cursor 分页及分页 SQL 的 HTTP context 传递已在第 7 节完成；继续测量大目录和频繁写入下的实际成本。
2. 按角色制定数据库连接与 worker 并发预算，接入 AI 连接池观测；结合实际积压调整调度间隔。
3. 继续验证 MinIO create 已成功但 provider ID 尚未持久化的歧义窗口、真实 relay 进程退出/恢复、完整上传 HTTP 故障、Redis broker 中断恢复。
4. 完整 staging 部署后执行目录、上传、混合搜索压力测试，记录 p50/p95/p99、错误率、资源与恢复时间。

不得把本轮功能 smoke、单次 SQL 延迟或测试数量转换为容量承诺。迁移 22 回退前必须停止新 relay，并处理仍为 sending 的 delivery；当前 down 脚本会拒绝含 sending 行的旧状态约束，不能直接在线回退。

## 7. 目录分页增量验证

证据目录：[20260911T075328Z-1381869](artifacts/high-concurrency/20260911T075328Z-1381869/)。本节覆盖分页增量，前文保留上一轮的验证记录。

### 实现和修复

- HTTP children 默认返回 50 项，显式 limit 仅接受 1..100；支持名称升/降序，始终目录优先、名称与 ID 稳定排序。
- 修复文件游标后升序重复返回目录的问题；将 `Raw` 后追加条件改为 GORM 可组合查询，确保 cursor、ORDER BY 和 LIMIT 真正进入文件 SQL。
- 保留 PostgreSQL 排序结果，不再用 Go 字符串排序重排数据库结果，避免 collation 差异。
- cursor 绑定 owner/folder/sort/content epoch/ACL epoch；非法 cursor 为 422，版本变化为 409。扩大 token 长度上限，以容纳合法的 255 字节名称及 JSON 转义。
- 首页缓存的 key 使用本次读取的 epoch，返回前再次校验版本；分页查询和版本读取使用 HTTP context。
- Drive 每次 50 项，取消旧请求、忽略晚响应，过期 cursor 自动回到首页；图片预览每批最多并发 6 个。
- 移动目录树提供后续目录入口，看到文件即不再请求目录页；关闭弹窗和切换选择取消旧请求，过期 cursor 重载根目录。
- OpenAPI 文档同步真实参数与 409/422 行为。现有非分页 `Children` 内部方法保留，HTTP 已切换到 `ChildrenPage`。

### 实际结果

| 验证 | 结果 | 证据 |
| --- | --- | --- |
| 全量 Go，真实 PG/Redis/MinIO | 225 通过，1 跳过，47 包；包含子测试 | `tests.jsonl`、`summary.json` |
| cache/drive/outbox/dbmetrics/upload race | 74 通过，5 包，无跳过 | `race.jsonl` |
| vet、迁移往返、双 API smoke、Prometheus 规则测试 | 全部通过 | `vet.log`、`migration.log`、`http-cache-smoke.log`、`alerts.log` |
| PostgreSQL 分页及 Gin HTTP | 两种方向、多个页长、作用域、epoch、缓存失效、可见性、取消及 422/409 通过 | `TestChildrenPage*` |
| 前端 Jest | 42 通过，14 套件 | `web-tests-recheck.log` |
| 前端 lint / production build | 通过 | `web-lint.log`、`web-build.log` |
| Playwright 真实 HTTP | 55 个目录首页 50 项，第二页完整；第二页目录可选且移动成功 | `browser-check.log`、`desktop-move-picker.png`、`mobile-move-picker.png` |
| Playwright stale cursor | 新建第 56 个目录后，旧页返回 409、自动重载 50 项；再次加载共 56 个唯一项 | `browser-check.log`、`desktop-directory.png`、`mobile-directory.png` |

前端首轮 4 个测试因图标加入可访问名称而失败，修正新按钮的装饰图标和根目录断言后全量通过；失败日志 `web-tests.log` 保留。浏览器日志仅含预期的未登录 refresh 401、主动测试的非法 cursor 422 与过期 cursor 409，没有观察到应用 JavaScript 异常。浏览器工具内写日志的 Node 模块调用不可用，随后通过工作区工具保存了结果，不影响先前已完成的交互断言。

### 运行状态和边界

- 原开发数据库现场复核仍为 `21|f`，原应用容器、配置和数据未发布或清理。
- 新增隔离数据库 `hc_20260911_075331_1381869`、`hc_20260911_075331_1381869_race` 保留；独立 Compose 依赖仍运行以支持预览。
- 预览入口 `http://127.0.0.1:3001`，后端 `127.0.0.1:18091`；使用本轮构建的 frontend 和 API，AI 关闭且没有启动 worker。
- 预览前端 PID `1417079`，API PID `1414853`。先核对 PID 对应进程，再用 SIGTERM 停止；停止隔离依赖使用 `docker compose -f deploy/compose.high-concurrency.yaml stop`，不要删除 volume。
- 浏览器测试账号 `hc-pages-70c8a2ef`，密码 `HcPreview-20260911!`，仅用于上述隔离预览。文件 `pagination-probe.pdf` 是用于目录移动的数据库元数据夹具，没有真实对象内容，不代表上传/下载通过。
- 唯一 Go 跳过项仍为真实 ES/Qdrant 集成。未做 staging 大目录压力、频繁 owner 写入下分页重启成本、角色预算、生产部署或容量验收。本节重跑的 EXPLAIN 仍是原热点 SQL，不是分页查询的容量证据。
