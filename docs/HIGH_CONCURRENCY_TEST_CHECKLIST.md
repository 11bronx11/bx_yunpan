# 高并发专项测试清单

- 日期：2026-09-11
- 对应计划：[HIGH_CONCURRENCY_UPGRADE_PLAN.md](HIGH_CONCURRENCY_UPGRADE_PLAN.md)
- 原则：每条测试都对应一个失败窗口、数据不变量或资源争用问题；不以覆盖率数字代替结论

## 1. 当前已执行

- [x] `go test ./...`
- [x] `go vet ./...`
- [x] `go test -race ./internal/platform/cache`
- [x] upload、outbox、storageops、cache、drive、bootstrap focused tests
- [x] 真实 PostgreSQL 下执行 upload/outbox/storageops focused tests
- [x] `000022` up/down 迁移验证，开发数据库已恢复到迁移 21
- [ ] 真实 MinIO/Redis staging 故障窗口
- [ ] staging 容量 Smoke
- [ ] 指标和告警验证

### 2026-09-11 隔离验证增量

证据以 [验证报告](HIGH_CONCURRENCY_VALIDATION_REPORT.md) 和 `artifacts/high-concurrency/20260911T071039Z-1206723/` 为准。下方 staging 项不因隔离用例通过而自动勾选。

- [x] 独立 PostgreSQL、Redis、MinIO 环境；原开发库保持 21，独立库 22 -> 21 -> 22。
- [x] 全量测试 207 通过、1 跳过、47 包；跳过项为真实 ES/Qdrant。
- [x] cache/drive/outbox/dbmetrics/upload race 共 56 通过；最后积压年龄修改另跑 dbmetrics race 通过。
- [x] 旧 Outbox ACK/fail 不覆盖新 token；真实 Redis 入队成功、数据库未 ACK 的状态模拟恢复。
- [x] Redis 暂停 1 秒，约 51ms 回源；真实 L2 读取、client 不可用回源。
- [x] 两个独立 API 的 HTTP 创建、重命名、删除跨副本立即可见，12 并发读正确。
- [x] 文件详情命中缓存仍拒绝其他 owner 或 non-ready object。
- [x] 真实 MinIO complete 后 PG 状态更新故障，单任务重试收敛；事件、文件、引用、配额不重复。
- [x] 缓存、Outbox/storage backlog、租约接管、API/分离 worker SQL pool 指标已接入。
- [x] 两个 API 实际 `/metrics` 输出检查；Prometheus 规则语法及积压/采集故障触发和恢复测试。
- [x] 5 万行合成数据 EXPLAIN，报告记录实际索引选择，未强制关闭 seqscan。
- [ ] storage 实际混合 claim 使用新增 ready/lease 索引：当前选用旧 claim 索引，诊断子查询可用新索引。
- [ ] 混合搜索压力、完整上传 HTTP 故障、provider create 歧义窗口、broker 中断恢复、真实 relay kill。
- [ ] 完整 staging 部署、Prometheus 采集与通知链路、角色预算及容量报告。

勾选项只代表已实际执行，不代表生产容量已经证明。

### 2026-09-11 目录分页增量

证据：`artifacts/high-concurrency/20260911T075328Z-1381869/`；验证报告第 7 节。

- [x] 真实 PostgreSQL 下，两种排序及 1/3/5/8/100 页长遍历无重复无遗漏，包含目录/文件边界和最长文件名。
- [x] cursor owner/folder/sort 作用域校验；content/ACL epoch 变化失效；另一个 service 的写入使首页缓存失效。
- [x] HTTP 参数边界、422 非法 cursor、409 过期 cursor；分页查询接收取消的 context。
- [x] Drive 首屏有界、按需追加、旧目录晚响应不覆盖当前目录、卸载取消、409 重载首页。
- [x] 移动目录树分页、遇到文件后停止请求目录页、旧目标 breadcrumb 晚响应不覆盖当前选择。
- [x] 隔离全量 Go 225 通过/1 跳过、race 74 通过、前端 42 通过；vet/lint/build 通过。
- [x] Playwright 1440x1000 / 390x844，真实 HTTP 创建 55 个目录、分页、移动至第二页目标、409 自动恢复，无横向溢出。
- [ ] staging 大目录查询计划、频繁写入下的分页重启成本和容量。

## 2. P0 在线路径

### 上传创建

- [ ] MinIO create 延迟或不可用时，HTTP 仍在请求超时预算内返回 `initializing`。
- [ ] 同一幂等键并发请求只产生一个 session 和一个 create operation。
- [ ] 请求事务提交后进程退出，storage worker 能继续完成 create。
- [ ] create worker 超时重试不会产生第二个 provider upload。

### 上传完成

- [ ] 并发 complete 只允许一个请求切换到 `completing`。
- [ ] 只产生一个 `multipart.complete` operation。
- [ ] 请求阶段不调用 MinIO complete，不进行长时间轮询。
- [ ] complete worker 在 provider 成功后只推进一次校验和最终状态。
- [ ] provider 成功但数据库更新失败时，重试能收敛且不重复创建 Entry。

### 取消和过期

- [ ] 并发 abort/expire 只释放一次配额。
- [ ] abort operation 至少一次执行，重复执行不会破坏已完成对象。
- [ ] 过期清理不会抢占正在 complete 的会话。

## 3. Outbox 和队列

- [ ] claim 事务不持有数据库事务调用 Redis/Asynq。
- [ ] Redis 入队成功、ack 前进程退出，下一轮使用同一 TaskID 重试。
- [ ] 重试后队列中只有一个可执行任务，消费端重复执行仍幂等。
- [ ] `sending` 租约过期后由另一副本接管。
- [x] 旧 lease token 的 ack/fail 不能覆盖新副本状态（真实隔离 PostgreSQL）。
- [ ] Redis 不可用时 delivery 保持可重试状态，数据库事务不长时间阻塞。
- [ ] AI disabled 时历史 AI delivery 不会长期占用 relay 扫描窗口。

## 4. 业务缓存

- [ ] 目录 children 首次请求回源，后续请求命中 L1。
- [ ] L1 淘汰后可从 Redis L2 读取。
- [ ] Redis 命中后回填 L1。
- [ ] 同一 key 并发 miss 只产生一次 PostgreSQL 回源。
- [ ] Redis 超时不阻塞请求超过 50ms，并能回源 PostgreSQL。
- [ ] Redis 完全不可用时目录和文件详情仍可用（JSONStore 回源已验证；完整故障 HTTP 尚未执行）。
- [x] 创建、重命名、移动、删除后通过 committed epoch 使旧 key 失效，另一副本下次读取新数据；含 worker 完成上传。
- [ ] 不同 owner 的相同 folder/file ID 不会复用缓存结果。
- [x] quota、权限事实、reference count、最终上传状态不从普通缓存读取；文件缓存命中仍作 owner/状态校验。

## 5. 数据库索引

对 staging 数据库执行 `EXPLAIN (ANALYZE, BUFFERS)`，至少确认：

- [ ] active upload 扫描命中 `idx_upload_sessions_active`。
- [ ] 同目录 active filename 查询命中 `idx_upload_sessions_name_active`。
- [ ] storage operation ready 扫描命中 `idx_storage_operations_ready`。
- [ ] running operation lease recovery 命中 `idx_storage_operations_lease`。
- [ ] Outbox ready/lease 扫描分别命中 ready 和 lease partial index。
- [ ] 文件名模糊查询使用 `idx_file_entries_name_trgm`，没有退化为全表扫描。
- [ ] object GC 和 async task recovery 使用对应 partial index。
- [ ] 索引不会改变权限过滤条件；查询计划仍先限制 owner/可见范围。

## 6. 搜索读路径

- [ ] 纯文件名搜索在查询上限内完成，并遵守候选数限制。
- [ ] embedding/query cache 命中时不重复调用 embedding provider。
- [ ] PostgreSQL、ES/Qdrant 任一派生索引不可用时，接口返回可控错误或降级结果。
- [ ] 外部索引结果仍经过 owner/权限过滤。
- [ ] 索引版本不匹配、tombstone 缺失时不会返回过期文件。
- [ ] 同一用户的并发搜索不会无限创建 provider 或数据库请求。

## 7. 资源争用和压力 Smoke

只在 staging 执行，必须记录环境、数据量、并发、p50/p95/p99、错误率和资源使用：

- [ ] 目录列表与文件详情混合读。
- [ ] 创建 UploadSession 与幂等 Complete 混合请求。
- [ ] 混合文件名/全文/向量搜索。
- [ ] relay、storage worker、AI worker 同时运行时的数据库连接占用。
- [ ] Redis cache、broker、PostgreSQL 任一组件短暂故障时的恢复时间和积压。

这些结果只说明当前 staging 配置的边界，不直接转换成生产 QPS 承诺。

## 8. 发布门禁

### 合并前

```text
gofmt -w 受影响 Go 目录
go vet ./...
go test ./...
go test -race ./internal/platform/cache ./internal/upload ./internal/outbox
```

### staging 发布前

```text
迁移 up
索引 EXPLAIN
真实 PostgreSQL focused tests
真实 MinIO/Redis 故障窗口
三类性能 Smoke
```

### 判定规则

- 权限泄漏、重复扣配额、重复最终对象、旧租约覆盖新状态：直接失败。
- Redis 故障可回源但延迟升高：记录告警，不判定数据错误。
- 队列有积压但可通过租约和重试收敛：记录容量问题，不能当作功能通过。
- 没有真实依赖或 staging 证据的项目，标记“未验证”，不能写成“已通过”。
