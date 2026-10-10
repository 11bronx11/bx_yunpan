# 全局分布式审查与运行验证（2026-09-11）

后续修复和最新验证见 [分布式问题修复与验证（2026-09-12）](DISTRIBUTED_FIXES_20260912.md)。本文保留 r4 的历史状态和指标；其中外部索引、canonical 清理及迁移验证的状态以新报告为准。

状态：本轮代码、镜像、隔离 staging 全量测试和开发环境切换已完成。本文件不构成跨主机生产高可用或外部索引切换通过声明。

## 审查范围与基线

- 当前工作树包含用户前几轮未提交的架构、高并发、AI/RAG 升级。本轮在这些改动上修复，没有回滚用户代码。
- 重点检查 PostgreSQL 事务与 Outbox、Asynq 投递、存储操作租约、MinIO 外部副作用、对象 GC/复活、目录并发、分享权限、缓存 epoch、索引投影和升级兼容窗口。
- 基础环境：单台 ARM64 VM，4 vCPU、约 5.2 GiB RAM；开发环境与隔离测试环境共址。容量结论不能外推多主机或生产模型。
- 隔离环境 `bx-yunpan-staging`，入口 `http://127.0.0.1:3002`，2 API、2 Relay、独立 object/media/maintenance/search/AI query/AI indexer；PostgreSQL、MinIO、etcd 和拆分 Redis。
- AI 使用 fake provider，检索主路径为 PostgreSQL/pgvector，外部 ES/Qdrant 不承担生产读取。开发环境的真实模型密钥没有写入测试材料。

## 已修复问题

| 问题与失败窗口 | 修复和验证 |
| --- | --- |
| 上传幂等键复用时请求参数变化，可能返回不属于该请求的旧会话 | 幂等键绑定目录、文件名、哈希、大小等参数，冲突返回 409；跨 API 实测重复创建及参数冲突 |
| 上传事务已成功提交，但进程在安排 staging 删除前退出 | 成功和失败事务内持久化 staging.delete，外部删除可重试；数据库回归和真实上传验证 |
| multipart 创建成功、数据库 ACK 前退出，以及取消与创建并发 | 重试先查询对应 incomplete upload；取消后对迟到的 provider 结果补安排 abort；真实 MinIO 回归 |
| 新存储 operation 缺少及时通知，正常请求等待扫描周期 | 创建/完成/取消/过期路径同事务写 operation 与 Outbox 通知 |
| 旧 GC 外部删除迟到，删除重新上传的同内容对象 | 对象复活使用代际物理 key；删除操作固定 SourceKey；deleting 与 operation 同事务，最终提交复查代际和 lease |
| 旧缩略图生成与对象复活交错 | 缩略图 key 绑定 canonical key 摘要；发布前锁定并复查对象代际 |
| Kafka 发布时持有数据库事务，发布后的 ACK 缺乏可靠 fencing | 短事务 claim、事务外 publish、独立 lease-token ACK/失败，过期 sending 可重领；伪 publisher 故障回归 |
| 升级期旧事件只存在部分 destination，后续路由永久缺失 | 按 destination 补全 legacy 投递；修复版本化 GC task 未应用延时 |
| 创建/移入目录与删除并发；互相移动目录成环 | 目标目录 SHARE 锁；目录移动 owner 级 advisory lock；10 组双 API 互相移动测试 |
| 分享导入幂等目标不一致，或撤销与导入交错 | 校验目标目录，事务内锁定并复查分享与源文件，补齐 ContentEpoch；并发导入、撤销和权限测试 |
| Worker 未传播升级 flags，异步事务缺失新事件/搜索路由 | Asynq BaseContext 显式携带 V2/Search/Kafka flags |
| 前端不认识 initializing/completing，导致错误重传 | 初始化轮询、恢复 completing 时仅等待；前端回归及浏览器上传验证 |
| GORM Updates 改写结构体后再读取“旧名称/旧目录” | 更新前保存旧值，验证 folder/file rename/move 的事件内容 |
| 固定恢复键让第二次 broker 丢失后的恢复永久去重 | 恢复扫描按上次已发布事件生成新恢复事件，设置宽限期，pending/failed/sending 不重复生成；连续三次丢失状态回归 |
| 持续写入使无游标首屏也返回 cursor_stale | 检测竞争后用只读 REPEATABLE READ 事务读取完整首屏；跨页仍保留 epoch fencing。修复前混合测试 6,993/24,076 请求失败，修复后预检 7,017 请求、702 写入、0 错误 |
| Relay 每批后固定等待 1 秒，把处理能力限制为配置的 20 条/秒 | 满批成功时继续处理下一批，批间检查关闭与失去领导权；一小时 idle interval 的回归要求三批立即完成。实测修复前 2,422 条目录事件使新上传初始化等待约 101 秒 |
| 单 API 停机的 Nginx DNS/连接窗口 | DNS 缓存与连接超时缩短为 2 秒，首次失败暂时摘除节点；仍须记录瞬时失败，不能承诺无损切换 |

## 已确认的剩余风险

1. **外部 ES/Qdrant 乱序覆盖，阻塞外部索引生产读取切换。** 真实容器探针先写新 tombstone，再重放旧 upsert，最终 ES 为 `file_version=1,tombstone=false`，Qdrant 为 `index_epoch=1,tombstone=false`。当前 adapter 没有服务端单调版本条件。PostgreSQL 权限和当前文档版本复查仍必要，但不能恢复被覆盖的召回。需要分别设计 ES 条件写和 Qdrant 的代际隔离/顺序化发布后，再验证启用。
2. **S3 外部副作用仍为至少一次。** lease fencing 保护数据库 ACK，无法撤销已发给 MinIO 的调用；incomplete-upload 查询缩小重试窗口，并不等于 provider exactly-once。
3. **单 API 硬停可能有瞬时 504。** 初次演练真实出现；超时调整后也观察到一次。故障测试要求最终收敛并记录错误，不将重试后的成功解释为全程无错误。
4. **基础设施单点仍在。** 本轮没有验证跨主机网络分区、PostgreSQL 主从切换/PITR、多节点 MinIO 或 etcd quorum 故障。容器副本测试不等于生产高可用认证。
5. **Kafka 运行链路没有实测。** publisher/dispatcher 的测试通过，但当前运行环境没有完整 Kafka broker/consumer 接线；不能宣称 Kafka 全链路通过。
6. **大库升级的锁时间仍需单独验证。** migration 000022 在事务内创建普通索引，当前小库升降级通过不代表大表在线建索引无写阻塞。大库应安排受控窗口或另行设计 CONCURRENTLY 迁移。
7. **Canonical 孤儿空间回收仍有静态风险。** `upload/service.go` 的 Verify 在业务提交前执行 ensureCanonical；若复制成功后因名称冲突或文件已存在而永久失败，staging 会回收，但复制目标可能没有对应 file_objects 行，现有 GC 按 file_objects 扫描，无法覆盖该目标。本轮未对这个分支做真实故障注入，不能把最终引用/配额检查解释为完整 S3 孤儿扫描通过。

## 测试证据

- 最新完整隔离回归：`artifacts/high-concurrency/20260911T121334Z-1439979/`。
- Go：236 passed、1 skipped、47 packages；race：84 passed、0 skipped、5 packages。
- 迁移 up/down/up、EXPLAIN、go vet、Prometheus rules、双 API 缓存 HTTP 检查通过。
- 前端：44 tests / 14 suites，ESLint 与 production build 通过；构建日志记录依赖废弃警告，未改动依赖版本。
- 开发库备份已在独立 `audit_restore_20260911` 数据库实际恢复并执行 21 -> 22；22 用户、21 有效文件保持一致，证据为 `restored-upgrade.log` 和 `restored-upgrade-state.txt`。这不是 PITR 或跨主机灾备验证。
- ES/Qdrant adapter 实测日志：`artifacts/global-audit/20260911/external-index-test.log`。
- 外部索引乱序复现：同目录 `external-window-probe.mjs`、`external-window-result.json`。
- 首屏/积压修复后的预检：同目录 `mixed-r4-recheck.json`；构建同时运行，不能作为正式性能值。
- 浏览器验证：新用户注册、真实文本上传、校验完成、刷新后文件可见且已索引。截图见同目录 `browser-desktop.png`。
- 早期 `preliminary*` 目录保留失败与预检证据，不能当作最终全量 PASS。

## 容量与性能

最终结果（`artifacts/global-audit/20260911/final/runtime-results.json`）：同机闭环并发客户端，延迟包含完整 HTTP body 读取；AI 使用 fake provider。

| 场景 | 并发 | 吞吐 | p50 / p95 / p99 | 失败 |
| --- | ---: | ---: | ---: | ---: |
| 1k 首屏 | 8 | 2,548 RPS | 2.6 / 6.4 / 9.9 ms | 0 |
| 1k 首屏 | 32 | 1,954 RPS | 11.7 / 49.2 / 73.5 ms | 0 |
| 10k 首屏 | 8 | 2,646 RPS | 2.6 / 5.9 / 8.9 ms | 0 |
| 10k 首屏 | 32 | 1,822 RPS | 12.8 / 51.6 / 76.1 ms | 0 |
| 50k 首屏 | 8 | 2,136 RPS | 3.0 / 7.3 / 13.3 ms | 0 |
| 50k 首屏 | 32 | 1,635 RPS | 14.1 / 58.4 / 89.9 ms | 0 |
| 50k 首屏 | 64 | 2,200 RPS | 26.2 / 56.8 / 78.3 ms | 0 |
| 50k 首屏 | 128 | 2,524 RPS | 46.4 / 96.1 / 125.2 ms | 0 |
| 50k 深分页 | 16 | 147 RPS | 101.6 / 193.2 / 262.4 ms | 0 |
| 50k soak（120 s） | 64 | 909 RPS | 55.9 / 163.4 / 297.1 ms | 0 |
| 混合读写（20 s） | 16 | 684 RPS | 11.2 / 78.7 / 127.9 ms | 0 |
| 下载 1 MiB | 8 / 32 | 100 / 296 MiB/s | 56.5 / 208.2 / 386.2 ms；94.3 / 196.4 / 319.2 ms | 0 |
| 混合搜索 | 1 / 4 / 8 | 313 / 203 / 355 RPS | p99 7.1 / 66.7 / 81.6 ms | 0 |

上传（含下载哈希校验）1 MiB：1/4/8 并发分别为 0.288/1.040/1.664 MiB/s，p99 为 4.86/4.71/5.79 s；16 MiB 单文件耗时 3.09 s。目录游标扫描 1k/10k/50k 均完整无丢项或重复。最终完整性断言 `reference_mismatches=0`、`active_entries_in_deleted_folders=0`、`unfinished_storage=0`、`quota_mismatches=0`、`outbox_pending=0`。

验收同时检查配额、引用计数、已删除目录中的活跃文件、未完成 storage operation、未发布 Outbox；没有提供长期生产 SLO 或理论最大容量保证。

## 镜像与部署

最终镜像标签为 `audit-20260911-r4`，构建日志：`artifacts/global-audit/20260911/build-final-verified.log`。关键镜像摘要：api `sha256:a6da1ab7a06e5825e072e30901fce9424fd234ca51929e5eb9e40fcaf3a9f7dc`，aisvc `sha256:8eaefdca651d246c1d1eadc5b265f5fe4a5883cd7552c4c9fd4b750fa67d9a48`，web `sha256:b0670fe740747410daf26d9e6bda1804c7ec01cebc2c89755b4afd13ef33704c`。构建采用单镜像串行、1 CPU、1536 MiB 内存上限；中途标签 `audit-20260911`、`r2`、`r3` 均不是最终发布依据。

开发环境已执行 `./deploy/up.sh --no-build --distributed-workers --ai-split --redis-split --search-indexer --api-replicas 2 --aisvc-replicas 2`，`deploy/.env` 为 `APP_IMAGE_TAG=audit-20260911-r4`。`deploy/config-check.sh` 和 `deploy/status.sh` 均通过；双 API、双 AI query、各 worker、search-indexer、Web、PostgreSQL、MinIO、etcd、拆分 Redis 均健康，入口为 `http://127.0.0.1:3000`。

本轮构建中间层曾导致磁盘不足，已按构建日志定向回收本轮无标签编译层；没有删除用户数据卷。开发环境切换后的镜像、运行数据与配置已通过 `config-check.sh`、`status.sh` 和健康探针复核。
