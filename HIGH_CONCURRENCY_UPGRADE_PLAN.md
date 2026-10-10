# 高并发核心链路升级计划

- 日期：2026-09-11
- 范围：`core-api` 在线请求、上传、Outbox、Redis 业务缓存、PostgreSQL 热点索引、搜索读路径
- 原则：先改路径，再用缓存和索引承接读压力；PostgreSQL 仍是业务事实源

## 1. 这份计划解决什么问题

当前高并发风险不只在数据库查询慢，主要在在线请求把外部副作用、重复查询和长事务串在了一起：

```text
浏览目录 -> API -> PostgreSQL
文件详情 -> API -> PostgreSQL
创建上传 -> API -> PostgreSQL + MinIO
完成上传 -> API -> PostgreSQL + MinIO + 校验
业务事件 -> PostgreSQL 事务 -> Redis/Asynq
搜索 -> PostgreSQL/派生索引 -> 权限过滤
```

本轮目标是把在线路径收敛成“校验、写事实、发任务、返回”，把 MinIO、索引投影和重活移到可重试的 worker；缓存只保存可重建读模型。

## 2. 当前状态

| 工作包 | 状态 | 事实 | 下一步 |
| --- | --- | --- | --- |
| 上传创建、完成、取消异步化 | 已写代码，部分真实故障验证通过 | 补单任务 succeeded 状态收敛，真实 MinIO complete 后数据库失败可恢复 | 完整 HTTP 故障、create 歧义窗口 |
| Outbox Relay 短事务 | 真实 PG/Redis focused 验证通过 | 旧 ACK/fail token fencing、稳定 TaskID 重投通过 | 真实进程退出及 broker 故障演练 |
| 目录/文件业务缓存 | 隔离双 API HTTP 验证通过 | 版本化 key 跨副本失效，Redis 50ms 预算；目录首页缓存 | 真实负载收益测量 |
| 热点数据库索引 | 隔离迁移和 EXPLAIN 完成 | 5 万行合成数据，实际索引选择见验证报告 | 正式 staging 迁移、真实数据复核 |
| 目录分页 | 实现和隔离验证通过 | 有界 keyset、422/409、取消旧请求、目录树按需加载；Go/浏览器验证通过 | staging 大目录及频繁写入场景测量 |
| 连接池和 worker 配额 | 未开始 | 当前没有按角色明确预算 | 按 API、relay、storage、AI 分开压测后调整 |
| 生产容量结论 | 未开始 | 本地单机和小数据不代表生产能力 | staging 压测后再给容量数字 |

## 3. 核心链路设计

2026-09-11 的复现、修复、实测边界和下一阶段见 [隔离验证报告](HIGH_CONCURRENCY_VALIDATION_REPORT.md)。本地独立依赖不等于完整 staging 发布。

### 3.1 目录和文件读取

1. 先查 L1，再查 Redis L2。
2. 缓存未命中时只允许一个请求回源，其他请求复用 singleflight 结果。
3. Redis 超时上限 50ms，失败直接回 PostgreSQL，不把 Redis 故障传递给用户请求。
4. key 带 `owner_id` 和 PostgreSQL 提交的 content/ACL epoch；命中前仍查询数据库版本。
5. 创建、重命名、移动、删除在业务事务内推进 epoch，所有 API/worker 提交后共同失效；局部删除只作补充。
6. 权限事实、配额、引用计数、最终上传状态不进入普通业务缓存。

目录 children 已使用 cursor 分页，默认 50 项、最多 100 项。目录优先，名称/ID 在数据库内稳定排序；cursor 绑定 owner/folder、排序及 content/ACL epoch。数据变化返回 409，客户端重载首页；后续页不缓存。下一步测量大目录及频繁写入导致的分页重启成本。

### 3.2 上传链路

创建请求只做：参数校验、幂等检查、数据库事务写入 `upload_sessions` 和 `storage_operations(multipart.create)`，然后返回 `initializing`。

完成请求只做：锁定会话、校验分片、切换 `completing`、写唯一 `multipart.complete` 操作。MinIO complete、对象校验和最终状态迁移由 worker 完成。

取消和过期清理只做状态迁移、配额释放和 abort operation；MinIO abort 由 worker 执行。

不变量：

- 同一个幂等键只产生一个业务结果。
- 同一个上传会话最多一个 create、一个 complete、一个 abort operation。
- worker 重试不能重复扣配额、重复创建 Entry 或删除仍有引用的对象。
- 外部存储调用不在 PostgreSQL 事务内。

### 3.3 Outbox 和任务队列

relay 事务只领取 ready delivery，设置 `sending`、`lease_until` 和 `lease_token` 后立即提交。Redis/Asynq 投递在事务外执行，之后独立事务 ack。

投递成功但进程在 ack 前退出时，下一轮依靠稳定 TaskID 去重；租约过期后可被其他副本接管；旧 token 不能覆盖新 worker 的结果。

backlog、sending、lease recovery 指标已接入。下一步按队列拆分 API、storage、AI 的并发预算，并验证部署后的采集与告警通知。

### 3.4 搜索和索引

PostgreSQL 继续负责权限过滤和最终可见性。ES/Qdrant（如启用）只保存可重建派生数据，不能直接决定用户是否有权访问文件。

本轮数据库索引覆盖：

- active upload 会话和同目录文件名冲突查询；
- storage operation ready/lease 扫描；
- Outbox ready/lease 扫描；
- file object GC 扫描；
- async task recovery；
- `file_entries.name_normalized` 的 trigram 查询。

下一步补齐：大目录分页、搜索候选上限和超时预算、外部索引 shadow 查询与切换门禁。

## 4. 执行顺序

### P0：把在线路径变短（当前已写代码）

- 保留 PostgreSQL 事务事实写入。
- 移除请求内 MinIO multipart create/complete/abort 和轮询。
- relay 改为短事务 claim + 事务外投递 + 独立 ack。

完成条件：单元测试、真实 PostgreSQL focused test、worker 重试测试全部通过。

### P1：让热点读可控（隔离验证完成，完整 staging 待发布）

- 部署 `000022` 迁移。
- 启用目录 children 和文件详情缓存。
- 增加 cache hit/miss、回源、Redis error、L1 eviction 指标。
- 用 `EXPLAIN (ANALYZE, BUFFERS)` 验证索引命中。

完成条件：Redis 故障时接口仍可用；索引扫描不再依赖全表扫描；缓存 key 无跨用户复用。

### P2：控制并发放大（未开始）

- 目录 children 改 cursor 分页。
- API、relay、storage、AI 分开设置连接池和 worker 并发。
- 对搜索、上传和任务队列设置超时、限流和 backlog 告警。

完成条件：单一路径的流量增长不会挤占其他路径的数据库连接和队列执行槽。

### P3：staging 故障与容量验证（部分隔离故障已验证，容量验证未开始）

- 真实 PostgreSQL、Redis、MinIO 环境执行故障窗口测试。
- 执行目录读取、上传幂等、混合搜索三类压力 Smoke。
- 记录数据量、并发、p50/p95/p99、错误率、队列积压和数据库连接使用。

完成条件：报告只给出当前环境容量边界，不把单机结果写成生产承诺。

## 5. 回滚边界

- `CACHE_ENABLED=false` 可关闭业务缓存并回源 PostgreSQL。
- `000022` 使用独立迁移，回滚前先确认没有代码依赖新增列和索引。
- 上传异步 worker 异常时保留数据库 operation，可暂停 worker 后人工重试，不在请求中恢复 MinIO 调用。
- 不删除数据库、Redis、MinIO 对象或队列数据作为回滚手段。

## 6. 不纳入本轮

- 不用缓存替代权限、配额、引用计数和最终状态。
- 不直接把 ES/Qdrant 当作权限事实源。
- 不在没有 staging 数据和故障证据时给出生产 QPS 或容量数字。
- 不为了覆盖率增加没有业务风险对应关系的测试。
