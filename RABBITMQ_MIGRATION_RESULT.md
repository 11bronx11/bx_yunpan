# RabbitMQ 替换交付记录：2026-10-08

运行时任务队列已由 Asynq 切换为 RabbitMQ，Kafka 执行代码和配置已移除。镜像已构建，原项目已经切换启动，组合与拆分角色都完成真实业务验收。本文记录实际执行结果；原 MIGRATION_PLAN / ASSESSMENT 保留为历史设计资料。

## 实现

- `internal/taskqueue`：持久化 quorum 队列、mandatory 发布、publisher confirm、手动 ACK、固定 TTL 重试档（1/5/30/60/300 秒）、quorum at-least-once dead lettering、DLQ、发布超时、重连、panic 捕获。
- object、media、maintenance、ai、search 五队列。handler 总数受 WORKER_CONCURRENCY 限制。原 Asynq 权重调度不等价保留；现在各队列消费者共享有界工作池，严格资源隔离使用独立角色。
- 所有业务 Handler、Worker/AISVC/Search 入口采用中立 Task 接口。Redis 保留缓存和协调职责；健康检查和 tracing 改为 RabbitMQ，broker 指标由 RabbitMQ Prometheus 插件提供。
- 复用 PostgreSQL Outbox 短事务 claim、SKIP LOCKED、lease token、ACK fencing。网络发布在数据库事务之外；历史 published Asynq owner 不会自动重复发布。
- GC 的 not_before 持久化，已确定的期限不因重试或重启提前。AI reprocess payload 中的业务 task_id 不再被 event ID 覆盖。
- task_results / task_failures 记录成功与终止失败。恢复扫描不自行复活 broker 中未完成的任务或死信；误删 broker/vhost/volume 后需要显式核对和恢复。
- 停机取消不消耗业务 retry 次数；后台 relay/reconciler 与消费者退出后才关闭数据库。
- Kafka producer/dispatcher/publisher、路由、context/config/Compose/部署检查及依赖已移除。历史迁移 SQL、文档和数据保留；Asynq 字符串只用于迁移工具、历史 ownership 和回归用例。

保证是 at-least-once。副作用提交与成功记录/ACK 之间崩溃仍可重放，依赖业务事务幂等、版本与租约，不能称为 exactly-once。

## 部署

11 个镜像均构建成功，tag 为 `rabbitmq-20261008`：migrate、migrate-asynq、api、worker、aisvc、search-indexer、outbox-relay、object-worker、media-worker、maintenance-worker、web。ID 见 `artifacts/rabbitmq-migration/images.txt`。全部串行构建；Dockerfile 一次编译各 Go 入口，使角色镜像共享编译结果。

本机 Docker Hub 和官方 Go proxy 超时，本次命令临时使用 docker.m.daocloud.io / goproxy.cn；仓库默认源未更换。

原项目 bx-yunpan：

- Web/API：http://127.0.0.1:3000，healthz、readyz、storage-healthz 通过。
- PostgreSQL：migration 23，dirty=false。组合 worker/aisvc 已启动。Search 投影保持原配置关闭。
- RabbitMQ 管理：http://127.0.0.1:15672。独立生成的密码在 deploy/.env，未输出到日志或报告。
- 宿主机已有 Redis 占 6379，项目 Redis 映射改为 6380，容器内仍用 redis:6379；宿主机服务未更改。
- deploy/.env 更新 tag、Redis 端口、RabbitMQ 账户，其他业务配置保留。deploy/config-check.sh 实测通过。

隔离项目 bx-rabbitmq-validation：Web 23000、Postgres 25432、Redis 26379、AMQP 25672/管理 25673、MinIO 29000/29001。最终验收使用数据库 rabbit_final、bucket rabbit-final、vhost final-validation；当前保留拆分角色运行，便于复查。旧卷副本容器已停止但未删除。

最终只读核验（2026-10-08 06:07 UTC）：原项目 9 个容器、隔离项目 14 个容器均 running；配置健康检查的容器均 healthy，所有运行中的应用镜像 ID 与本次 release tag 一致。两个站点 healthz、readyz、storage-healthz 均返回 200，配置一致性检查通过。原项目主队列和重试队列均为空，ai.dlq 有 5 条历史任务，与数据库 5 条 dead receipt / failure 记录一致。最新证据为 final-health.json、live-queues.json、live-migration.json 和两个项目的 runtime.json。

## 旧任务和备份

先以只读挂载的旧 Redis 卷副本重放 AOF 并盘点：redis-data 有 4 个 archived；redis-broker-data 有 142 个 archived、5 个 retry。原 PostgreSQL 为版本 22、461 条 Asynq published delivery。

在副本验证 dry-run、apply、重复 apply 后，原数据库完成迁移。5 个 retry 全部转入 RabbitMQ Outbox，保留原来的 5～8 次 retry、max_retry=8、超时与执行时间。456 条历史 published 保留 owner；archived 不自动重跑。

工具是 backend/cmd/migrate-asynq，默认校验，--apply 才提交；Redis 始终只读。本次源 Redis DB=0。工具拒绝 active、group/unique、绝对 deadline、无法关联 Outbox 的任务。000023 遇到 Asynq sending 行会拒绝切换，防止模糊发布窗口被静默重复执行。

导入器当前读取 Redis DB 0，保存旧 payload 的哈希；重新发布使用 PostgreSQL Outbox payload，未对两者做语义等价比较。本次 5 条任务均来自 DB 0，并已关联到已知 Outbox 事件。

5 个历史任务都指向两个 bucket=test 的旧测试对象。切换前副本中的 AI 文档已经是 failed/ai.storage_read_failed；原存储缺少 test bucket。5 个任务按照保留预算最终进入 DLQ，错误为存储读取失败或同对象处理租约冲突。原对象、旧消息和失败记录均保留，不能把它们描述为业务成功。证据：live-migration.json、legacy-object-before.txt、legacy-object-after.txt。

切换前备份在用户私有目录 `/home/bronx/.local/share/bx-yunpan-backups/rabbitmq-20261008/`，仅用户可读：

- original-before-000023.dump：原数据库 pg_dump custom 格式。
- deploy.env.before-cutover：原部署环境。
- backend-deploy.tgz：本次实现前 backend/deploy 工作区备份，包含此前脏改动。

000023 不提供自动 downgrade。回退需要停止写入/relay/consumers、核对已执行效果与队列并 drain/replay，再结合备份恢复，不能直接切回旧 worker，否则旧 Redis 任务可能再次执行。

## 验证结果

证据目录：`artifacts/rabbitmq-migration/`。

| 验证 | 结果 | 证据 |
| --- | --- | --- |
| 全量 Go 回归，真实 PG/Redis/MinIO/Rabbit | 251 通过，0 失败，5 跳过；随后新增的 kill 测试单独通过 | regression-complete.jsonl、regression-summary.json |
| Rabbit 传输与故障 | 5 队列、连续 confirm、unroutable、延迟重试、metadata、poison/panic/timeout/exhaustion、并发、停机重投、重连、broker 重启持久性通过 | transport.log |
| 强制终止消费者 | 未 ACK 消息以原 ID/原 retry 次数重投，通过 race | consumer-kill.log |
| 核心 race | taskqueue/outbox/recovery/worker 全通过 | race-release.log |
| Go vet | 通过 | vet-release.log |
| 前端回归 | 14 suites、44 tests 全通过 | frontend-regression.log |
| 组合角色 HTTP | 通过 | final/core-results.json |
| 拆分角色 HTTP | 通过 | distributed/core-results.json |
| 初次 5 分钟稳定性 + race | 14,974 confirmed / 14,974 unique consumed，0 重复、0 丢失 | stability.log |
| 最终 5 分钟稳定性 + race | 14,910 confirmed / 14,910 unique consumed，0 重复、0 丢失 | stability-release.log |
| Shell / JS / Compose / git diff --check | 通过 | 实际执行检查 |

HTTP 覆盖真实 multipart PUT/confirm/并发 complete、verification、下载 SHA256、权限隔离、AI index/reprocess task ID、hybrid search/ask（fake provider）、并发分享导入/撤销、PNG 缩略图/预览、GC 延迟与删除、broker 停止时 API 提交 Outbox、恢复处理、worker/relay 重启、20 次完整上传下载。两种模式最终五队列均有消费者，主/重试/DLQ 全空，每条 published 有成功 receipt，引用差异=0、业务终止失败=0。

全量回归跳过：Redis 专项故障预算、两项 ES/Qdrant 外部容器测试、broker restart、stability。后两项已单独执行通过；Redis 专项和 ES/Qdrant 本轮未覆盖。

中间失败日志保留：官方源超时；旧测试残留影响 claim（改为专属 schema）；单项测试原先共用业务 vhost（已改独立 vhost）；HTTP 主动停 broker 与其他集成测试并行导致连接重置（最终顺序执行通过）；receipt 先于 broker ACK 可见（检查改为等待队列清空）。Search backlog 并发处理出现过一次 PostgreSQL duplicate-key，消息随后重试成功，未出现终止失败。

## 复现与边界

常规启动：`./deploy/up.sh --no-build`，配置检查：`./deploy/config-check.sh`。只读旧卷盘点：`./deploy/rabbitmq-legacy-inventory.sh LEGACY_VOLUME`。

Go 集成测试需要在 backend 目录设置隔离 TEST_POSTGRES_DSN、TEST_REDIS_ADDR、TEST_RABBITMQ_URL、TEST_RABBITMQ_MANAGEMENT_URL、TEST_MINIO_ENDPOINT、TEST_MINIO_ACCESS_KEY、TEST_MINIO_SECRET_KEY 后执行 `go test -p 1 -count=1 ./...`。测试 fixture 使用专属 schema/vhost。

隔离 HTTP 复查：根目录执行 `RABBIT_TEST_VHOST=final-validation RABBIT_TEST_WORKER_ROLE=outbox-relay RABBIT_TEST_ARTIFACT_DIR=artifacts/rabbitmq-migration/recheck node deploy/rabbitmq-core-test.mjs`。该脚本限制项目名为 bx-rabbitmq-validation 前缀，包含 stop/start 故障。故障脚本必须与其他连接同一 broker 的测试顺序运行。

边界：单机 ARM64 Docker、单节点 RabbitMQ quorum，不是三节点 HA、跨主机分区、灾备或生产容量证明；稳定性为两段 5 分钟，不是 24h。AI 新任务流程用 fake provider；原站点保留 DashScope 配置，本轮不认证模型供应商额度和外部可用性。历史 test bucket 缺失需要数据恢复或另行确认清理。原有非本任务改动未回滚，未创建 Git commit。
