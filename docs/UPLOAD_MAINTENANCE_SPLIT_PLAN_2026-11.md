# 上传 Worker 与维护职责拆分

更新时间：2026-11-06（北京时间）  
用途：当前工作区实现与后续验证交接  
状态：上传执行独立；定时维护、恢复扫描和 GC 合并在 maintenance-worker；未部署

后续竞争、重试边界和真实 provider 验证见 [异步维护审计](ASYNC_MAINTENANCE_AUDIT_2026-11-06.md)。下方原拆分验证保留为历史证据；当前代码需要 migration 30，并限制自动恢复，不再无限新建任务。

## 当前职责

本次只将上传会话相关的异步操作从维护进程划出。周期维护与清理继续放在同一个 `maintenance-worker` 进程，不再单独部署恢复调度器。

| 角色 | 职责 | 队列 |
| --- | --- | --- |
| API | 上传会话创建、查询、分片确认、完成、取消；持久化会话、配额、storage operation 与 Outbox | 无 |
| upload-worker | multipart create/complete/abort，以及 canonical.ensure、staging.delete 的恢复执行 | upload |
| maintenance-worker | 定时过期/终止会话、释放预留配额、canonical orphan 检查、持久中间态恢复扫描；消费 GC、object.delete 与 variant.delete | maintenance |
| object-worker | 上传内容校验、正式对象入库；正常校验流程仍包含 canonical copy 与 staging 清理 | object |
| worker（combined） | 保留原本合并执行入口，兼容新 upload 队列和旧 maintenance 消息 | 多队列 |

上传会话 HTTP 接口仍在 API；本次划分的是后台执行职责。`ARCHITECTURE_UPGRADE_FINAL_EXECUTION_PLAN.md` 的 P2.2/P2.5 原先把 upload cleanup 与 reconciliation 放在 maintenance 内；当前继续保留这个维护进程，只增加独立上传执行角色。

## 调用链与维护循环

正常请求：API 在同一事务中写入会话、`storage_operations` 和 `upload.operation_recover.v1` Outbox 事件，relay 将任务投递到 `upload`，upload worker 重新读取并 claim 操作后执行。首次执行无需等待定时扫描。

`RunMaintenanceWorker` 在一个进程内启动以下三个周期 runner，并消费 maintenance 队列：

1. `runUploadCleanup`：收敛重试终止的会话并扫描过期会话；事务内锁 session、复查状态、释放配额、标记失败/过期并登记 abort/staging cleanup。实际上传清理由 upload worker 执行。
2. `runRecoveryReconciler`：恢复 verifying upload、media、AI、reprocess、GC 和未完成 storage operation；通过 PostgreSQL Outbox 补任务，由 relay 发布。
3. `runCanonicalOrphanReconciler`：检查失败/取消/过期上传留下的 canonical 对象，登记受 generation 保护的 object.delete 和 GC，由 maintenance 队列执行删除。

三个 runner 启动后先执行一次，后续共用 `UPLOAD_CLEANUP_INTERVAL`（代码默认 5m）和 `UPLOAD_CLEANUP_BATCH`（默认 100）。与队列消费共享进程生命周期、数据库连接池和 metrics endpoint。

周期扫描通过 `upload.NewRecoveryService` 只获得数据库维护所需能力，不直接执行上传 provider 操作。maintenance 进程自身仍持有队列与 MinIO 依赖，用于 GC/object.delete；上传执行与维护扫描之间通过 durable operation/Outbox 衔接。

## 保留的恢复与并发保护

- PostgreSQL 是会话和操作状态权威；外部存储调用允许 at-least-once，每次执行前 claim 租约，完成/失败写入带当前 `lease_token`。
- `recoverStorageOperations` 扫描 pending、到期 failed、租约过期 running，以及 multipart create/complete 已成功但会话仍在 initializing/completing 的操作。后一种情况由 upload worker 只补数据库状态，不重复已完成的 provider 操作。
- maintenance 收到旧 `storage.operation_recover.v1` 上传消息时只确认；其周期扫描使用新事件类型和 `upload.operation_recovery:<operation-id>:<type>` key 重新投递，避免旧恢复记录挡住新 upload 队列。
- storage task 重试耗尽进入 dead、但 operation 仍可重试且恢复家族未耗尽时，保留原 receipt，以 `:after:<旧事件ID>` 链出一个新 Outbox 事件，payload 的 RecoveryKey 同步更新。operation 最多尝试 12 次，恢复家族最多 3 个事件；上限先到即停止，保留审计记录并打日志/告警。
- Broker 已发布但执行 receipt 缺失时，复用原 delivery/task ID 重放；正常执行、退避中的任务不会被无条件复制。
- 多维护副本并发扫描时，通过事务 advisory lock、确定性事件身份与 recovery 唯一键去重；会话过期通过 session 行锁保证配额只释放一次，abort 使用稳定幂等键。
- canonical orphan 修复通过 session/object 锁、对象状态与 generation 检查保护共享对象，删除复用 object.delete/GC 路径。
- `RunOnce` 为六类扫描分配有界额度，并轮换起始类别，避免一个类别长期挤占扫描预算。
- `TryStartWithToken` 允许接管 running 且 lease_until 为空的遗留操作；旧 owner token 不能覆盖新 owner 的结果。

## 部署与回滚

- 新增角色只有 `upload-worker`；独立恢复调度器的入口、role、Compose service、构建目标、监控与检查脚本配置已移除。
- Compose、build/up、config/status/logs、Prometheus、audit 和 staging connection budget 保留 upload worker 配套。
- 升级需要支持新 upload 事件的 relay、maintenance worker 与 upload worker 就绪，再切换 API。maintenance 必须运行周期扫描，才能将旧上传恢复消息对应的未完成操作重新投递。
- 当前需要数据库 migration **000030_async_retry_bounds**，增加 dead storage state、variant.delete 和 recovery.stopped_at。原始职责拆分本身曾不需要 migration；后续竞争审计引入了这些变更。RabbitMQ topology 增加 upload queue。上线先处理旧消费者兼容和 schema，再启用新 worker；具体顺序见审计文档。
- `./deploy/up.sh` 不带 `--distributed-workers` 切到当前 combined worker；它保留周期扫描并处理新旧消息。更旧镜像可能不识别新事件，不能将旧镜像回滚当作已经验证的兼容路径。
- 不清空队列、Outbox、receipt、operation 或 MinIO 对象；核对未完成操作、pending/unrouted Outbox、重试/DLQ 和最老任务年龄。

## 验证与运行状态

原职责拆分阶段实跑结果（后续审计前的历史证据）：

- `go test -p 1 -count=1 ./...` 通过：52 个 package，243 项测试/子测试通过、128 项跳过；该轮未配置外部测试依赖。
- `go vet ./...`、`git diff --check`、`bash -n deploy/*.sh` 和变更 JavaScript 语法检查通过。
- 主/staging/audit 三套 Compose 配置解析通过；确认只保留 maintenance/upload，未声明独立恢复调度器。
- `node deploy/staging-budget-check.mjs` 通过：应用连接上限 72，加 migration 2、operations 16、superuser reserve 3，共 93/100，未分配 7。这是静态预算，维护扫描与 GC 共享 maintenance 的 4 条连接池上限，尚非容量测量。
- 临时、独立的 PostgreSQL 17/pgvector 容器中，为 recovery/upload/storageops/outbox 分别创建新库并执行现有 migration，再跑对应包的 `go test -race`：共 92 项测试/子测试通过、5 项跳过。覆盖恢复去重、旧 recovery key 共存、dead task 重建、成功 operation 的状态收敛、并发过期、canonical orphan 和租约 fencing。
- 过期与 orphan 并发用例改用无 provider 客户端的 `NewRecoveryService`；过期测试同时断言实际 abort 尚未调用，且只生成一个投往 upload 队列的 recovery delivery。
- PostgreSQL 回归暴露一处原有 Outbox 测试查询问题：复用带旧主键的 GORM 结果变量造成第二次查询条件冲突；改用独立变量后，Outbox race 回归 57 项通过、3 项真实 RabbitMQ 用例跳过。
- 隔离测试容器及其临时数据库已随测试移除；测试没有连接现有业务数据库。原始日志保存在 `/tmp/bx-upload-maintenance-r5ybdq86/`；修复后的 Outbox 日志在 `/tmp/bx-upload-maintenance-6mbiu1oc/`。

上述历史记录中的 5 项集成跳过分别需要真实 MinIO（2 项）和 RabbitMQ（3 项）。后续审计已补充这两种 provider 的隔离真实集成和竞争验证，结果与范围见新审计文档。

当前运行容器仍为既有版本，没有部署本次改动。数据库并发回归、真实 RabbitMQ/MinIO 故障注入、split/combined 切换与容量结论需要分别记录，不能由静态配置检查推断。

## 后续验证

1. 已完成隔离 PostgreSQL/MinIO/RabbitMQ 联调、双维护扫描器、租约接管、Broker 重启和 receipt 丢失恢复；后续只在新增变更或故障时补对应回归。
2. 真实镜像部署时另行演练 migration 30、旧消费者退出与 split/combined 切换；本轮未部署。
3. 部署后检查维护扫描时延、GC/上传队列积压、dead/stopped 告警、重试/DLQ 与 PostgreSQL 连接池；维护扫描与 GC 共享进程和连接预算，容量仍需实测。
