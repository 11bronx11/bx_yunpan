# 分布式问题修复与验证（2026-09-12）

本轮对应 `GLOBAL_DISTRIBUTED_AUDIT_20260911.md` 的剩余风险。代码修复、真实依赖回归、镜像构建、r5 staging 整体测试和开发环境切换已完成。开发库另外发现 2 条升级前已有的 test 桶引用计数异常，详见下文。本报告不构成跨主机生产 HA 或外部检索切换验收。

## 已修复的实际 bug

| 失败窗口 | 修复后的行为 | 证据 |
| --- | --- | --- |
| ES 旧 upsert/bulk 覆盖较新版本或 tombstone | 所有新写入使用服务端条件 `_update`；文件版本、ACL 版本和索引 epoch 不回退，同版本 tombstone 优先；删除先于首次插入仍保留标记 | `TestExternalTombstonesSurviveReorderedWrites`，真实 ES 8.19.0 |
| Qdrant 旧 upsert 覆盖删除状态 | 内容 ID 包含 chunk+epoch；删除记录使用独立 ID 空间且只追加；强顺序写、全部副本一致性读取删除记录，查询剔除旧代际，写后补标删除 | 同一真实测试，Qdrant 1.15.5；包含单条、批量、删除前后重放 |
| PG 投影同版本重放恢复已删除内容、删除未推进水位 | 记录删除版本/epoch，保留内容 Tombstone，保护 ACL 水位；默认 projector 从 `v9` 等配置解析实际 epoch | `TestPostgresProjectionRetryAndTombstone`、`TestProjectorParsesConfiguredEpoch` |
| GC 请求早于实际删除，或者取消后同内容重新引用 | 持 SHARE 锁复查对象；ready 对象不因旧 GC 请求被删除；只删除具体 chunk 代际，不永久屏蔽整个 object UUID；完成 GC 的事务补发投影事件 | `TestGCProjectionPreservesRevivedObjectGeneration`、真实上传/GC 回归 |
| canonical 复制成功，业务事务因同名冲突永久失败，目标无对象记录 | 在失败事务中认领未登记目标，直接进入 deleting 并持久化 object.delete；复用正常 GC，不直接删除可能共享的 key；历史终态 operation 可恢复 | `TestRealMinioCanonicalFailureCleanup`、`TestCanonicalLegacyOrphanEntersGC` |
| 已失败上传被旧任务再次执行 canonical 复制 | 创建 operation 前锁定并复查上传状态，执行前跳过终态会话；复制超时后认领的对象不会被当作 ready 文件复用 | `TestFailedSessionDoesNotRestartCanonicalCopy` |
| 后台维护/组合 Worker 丢失搜索与 Kafka 事件开关 | 定时恢复路径也传播 Search/Kafka flags，GC 完成和清理事件保留所需路由 | 全量测试和 split-worker staging 回归 |

外部查询限定目标索引和预期 epoch。删除记录查询失败时返回错误；被删除候选耗尽召回预算时也返回错误，由已配置的 PostgreSQL fallback 接管，避免静默接受不完整召回。

## 保证与升级边界

- 上述外部索引防乱序机制要求所有读写方使用新 adapter。ES 旧版原始 PUT 仍能绕过条件脚本；Qdrant 内容 ID 与删除协议已经变化。启用外部读取前必须使用新 target 重建、升级全部读写方，并验证追平和切换，不能让旧进程继续写同一目标。
- Qdrant 删除记录必须保留到整个 target collection 退役。物理内容 payload 不是唯一删除依据；查询必须经过新 adapter。当前测试不证明分片迁移、quorum 故障下的行为。
- PostgreSQL 仍是权限、配额、引用计数和当前文档版本的权威来源。外部投影保持最终一致，没有把 PG、ES、Qdrant、S3 变成跨系统原子事务。
- S3/MinIO 请求仍是至少一次。lease fencing 阻止过期任务提交数据库状态，代际 key 隔离旧删除；不能撤回已经发送的 provider 请求，也不保证 provider exactly-once。
- 对任意长延迟、在 GC 之后才生效的 provider 写入，仍需周期性 inventory 和补偿机制。此次修复覆盖持久 operation 可识别的 canonical 失败窗口，不能据此保证未来永不产生孤儿。
- 本轮没有新增或修改数据库 migration。开发和 staging 继续使用 PostgreSQL 检索路径。

## 真实验证

所有本轮证据保存在 `artifacts/distributed-fixes/20260912/`。

| 检查 | 结果 | 证据文件 |
| --- | --- | --- |
| 全量 Go，真实 PG/Redis/MinIO/ES/Qdrant | 247 个测试（含子测试）通过，0 失败、0 测试跳过；47 个包，其中 17 个无测试文件 | `tests-final.jsonl` |
| race：outbox/search/upload/objectstore/ai | 125 个测试（含子测试）通过，0 失败、0 跳过；5 个包 | `race.jsonl` |
| 静态检查 | `go vet -p 1 ./...`、`git diff --check` 通过 | 本轮终端检查 |
| MinIO 真实失败注入 | 确认复制已经成功，再触发名称冲突；GC 删除失败上传目标，原文件保留；同哈希重传使用新物理代际 | `TestRealMinioCanonicalFailureCleanup` |
| 上述测试桶全量扫描 | 2 个有效对象、0 未登记对象 | 同一测试日志 |
| 开发桶只读全量清单核对 | 27 个物理对象：19 canonical、8 缩略图；0 异常，元数据扫描前后相同 | `canonical-inventory.json` |
| r5 staging 整体/故障/容量测试 | PASS；8 组功能断言、7 类故障、24 项测量，耗时 622.4 s | `runtime/runtime-results.json` |

全量测试使用新数据库 `fix_20260912_final`，race 使用 `fix_20260912_race`。较早 `tests.jsonl` 使用含旧夹具的数据库，Outbox 全局 claim 取到无关记录而失败，保留为排查记录，不作为最终验收结果。测试执行器为宿主 Go 1.25.7，发布镜像使用 Go 1.25.13；镜像通过实际运行回归验证。

开发桶扫描脚本为 `node deploy/canonical-inventory-check.mjs`，只读核对当前已完成对象，不包含历史版本、未完成 multipart、全内容哈希或跨系统原子快照。本次在开发桶未发现孤儿，不代表未来或其他桶不存在。

真实 ES 测试期间曾因宿主磁盘水位阻止分配，在独立审查实例临时调整了水位阈值。测试后 transient 设置已恢复为空，ES/Qdrant 已恢复原来的停止状态；没有删除其数据卷。

运行测试包括双 API 幂等请求、目录互相移动防环、真实 multipart/重复 complete/下载 SHA256、分享并发导入与撤销、AI 索引与检索、真实 GC 后同哈希新代际上传和缩略图。7 类故障覆盖单 API、缓存、broker、MinIO、Relay 接管、AI query、AI indexer 积压；broker 恢复后待上传约 2.7 s 收敛，MinIO 约 40.7 s，Relay 接管约 1.2 s，4 个 AI 积压约 0.8 s。单 API 此次探针没有瞬时失败，不能覆盖此前已观察到的 504 窗口。

staging 最终引用计数、配额、已删除目录中的活跃文件、未完成存储 operation、未发布 Outbox 的异常计数均为 0。

## Migration 000022 锁窗口

`node deploy/migration-000022-lock-check.mjs` 在专用数据库执行原始未改动的 000022 SQL。file_objects、file_entries、outbox_events、outbox_deliveries 各 100,000 行；观察到 outbox_deliveries 的 `AccessExclusiveLock`，并发 UPDATE 实测 458.7 ms，未超过设置的 500 ms SQL 超时。观察总时长 632.4 ms，检查的索引有效。

这确认了实际写阻塞窗口，不能外推生产大表的最大锁等待。迁移把 ALTER TABLE 和普通 CREATE INDEX 放在一个事务中；尚未升级的大库需要维护窗口，或单独设计和演练并发索引迁移。不能直接把 CREATE INDEX 改为 CONCURRENTLY 后仍留在该事务中。

证据：`migration22-lock-results.json`。专用数据库 `lock22_1789187135908` 是降到 21 后直接施加原始 SQL 的锁探针，其 migration 元数据仍为 21，不作为正常迁移部署使用。

## 镜像与运行环境

标签：`audit-20260912-r5`，平台 `linux/arm64`。8 个后台二进制使用 Go 1.25.13、CGO_ENABLED=0、`-trimpath -ldflags='-s -w'` 串行编译；编译容器上限 1 CPU、1536 MiB。使用已有 r4 API 运行层，仅替换 `/app/service`，避免剩余磁盘被重复构建层耗尽。未改动的 migrate/web 镜像复用 r4 内容并标记 r5。

- 构建脚本与日志：`artifacts/distributed-fixes/20260912/build-runtime.sh`、`build.log`。
- 10 个镜像 ID、大小、创建时间和 225 个后端源文件的 SHA256 清单：`image-manifest.json`。
- 后端源清单摘要：`9937cc902932907131236f15221fbf7eca30f65182893e00bfb77043daa3d67b`。
- staging：`http://127.0.0.1:3002`，2 API、2 Relay、独立 Worker、AI query/indexer、拆分 Redis；r5 启动检查通过。
- 开发环境：`http://127.0.0.1:3000`，已切换 r5；双 API、双 AI query、独立各 Worker。`deploy/status.sh` 内的逐副本就绪检查、gRPC health、配置核对、Web/API/storage 探针和 Worker metrics 均通过。
- 启动与检查证据：`staging-up.log`、`development-up.log`、`development-status.log`、`deployment-state.json`。

收尾时宿主根文件系统约 98% 已用、可用 1.9 GiB。测试和旧镜像数据均保留；此余量不足以作为生产持续增长的存储容量保证，后续扩容或清理应另行处理。

开发库保持 schema `22|false`、22 用户、21 个有效文件；配额、目录关系、存储 operation 和 Outbox 无异常。全库引用计数有 2 条异常，均为 `bucket=test`、9 月 4 日创建的 ready 对象，记录为 1、实际文件引用为 0。9 月 11 日恢复库中存在相同 ID 和相同异常，确认早于本轮升级；实际业务桶 `yunpan` 的引用计数无异常。这两条历史记录保留，不能把全开发库计数报告为 0。证据：`development-integrity.json`、`development-reference-findings.json`、`restored-reference-baseline.json`。

## 容量与性能

本轮实际执行 `AUDIT_ARTIFACT_DIR=artifacts/distributed-fixes/20260912/runtime node deploy/global-audit-test.mjs`，2026-09-12 04:34:41 至 04:45:03 UTC。方法与上一轮相同：单机 ARM64 VM、4 vCPU、约 5.2 GiB RAM；同机闭环 HTTP 客户端，延迟包含响应体；AI fake provider，搜索使用 PG/pgvector。测试数据累计保留，数据库约 67 MiB、累计约 18.9 万目录；不能当作多机或真实模型性能。

| 场景 | 并发 | 吞吐 | p50 / p95 / p99 | 失败 |
| --- | ---: | ---: | ---: | ---: |
| 1k 目录首屏 | 8 | 1,646 RPS | 4.0 / 10.6 / 15.8 ms | 0 |
| 1k 目录首屏 | 32 | 816 RPS | 28.8 / 112.4 / 174.9 ms | 0 |
| 10k 目录首屏 | 8 | 1,205 RPS | 5.4 / 14.4 / 22.9 ms | 0 |
| 10k 目录首屏 | 32 | 816 RPS | 28.3 / 118.7 / 179.9 ms | 0 |
| 50k 目录首屏 | 8 | 1,235 RPS | 5.4 / 13.6 / 21.0 ms | 0 |
| 50k 目录首屏 | 32 | 731 RPS | 31.2 / 124.3 / 212.6 ms | 0 |
| 50k 目录首屏 | 64 | 1,065 RPS | 53.8 / 120.6 / 171.4 ms | 0 |
| 50k 目录首屏 | 128 | 1,230 RPS | 94.0 / 202.3 / 268.0 ms | 0 |
| 50k 深分页 | 16 | 102 RPS | 154.8 / 270.0 / 341.3 ms | 0 |
| 50k 持续负载，120 s | 64 | 1,035 RPS | 54.5 / 124.8 / 191.3 ms | 0 |
| 混合读写，20 s | 16 | 592 RPS | 12.5 / 96.3 / 165.5 ms | 0 |
| 下载 1 MiB，含 SHA256 | 8 | 162 MiB/s | 33.7 / 131.8 / 242.7 ms | 0 |
| 下载 1 MiB，含 SHA256 | 32 | 252 MiB/s | 110.1 / 256.1 / 360.7 ms | 0 |
| 混合搜索，fake provider | 1 | 283 RPS | 3.2 / 5.6 / 8.4 ms | 0 |
| 混合搜索，fake provider | 4 | 209 RPS | 16.2 / 40.8 / 58.8 ms | 0 |
| 混合搜索，fake provider | 8 | 302 RPS | 19.8 / 67.1 / 99.5 ms | 0 |

有吞吐计数的闭环 benchmark 共 304,765 次请求、0 失败，另有 610 次全游标分页请求；1k/10k/50k 扫描均无重复或丢项。1 MiB 上传含下载校验，1/4/8 并发分别测试 10/40/80 个文件，吞吐 0.172/1.052/1.949 MiB/s，p99 25.67/5.22/7.19 s；16 MiB 分片上传含下载校验耗时 4.63 s。

单并发上传出现一次 25.67 s 长尾。该会话的 multipart.create/complete 分别约 1.07/0.94 s，而 `object.verify_requested.v1` 从创建到发布等待 19.82 s，canonical.ensure 约 0.44 s。发生在混合写入阶段之后，显示了异步投递排队对上传完成时间的影响；最终成功并通过 SHA256。证据：`upload-tail-diagnostic.json`。此次验收未设置上传延迟 SLO，功能 PASS 不能解释成该长尾可接受。

相较 r4，50k 首屏 128 并发从约 2,524 降至 1,230 RPS，混合读写从 684 降至 592 RPS；120 s 持续负载从 909 升至 1,035 RPS。共享主机负载、缓存和累计数据未做严格 A/B 控制，不能据此确定变化原因，也不宣称无性能回退。以上为本轮真实指标，容量上限和长期生产 SLO 尚未确立。

## 仍待验证

1. 跨主机 PostgreSQL 主从切换、PITR、网络分区与恢复。
2. MinIO 多节点、etcd quorum、真实 Kafka broker 到消费者的完整链路。
3. ES/Qdrant 新 target 重建、混合版本退出、正式切换及多副本故障演练。
4. 生产规模大表迁移的最长阻塞及回退流程。
5. 任意长延迟 provider 写入后的持续 inventory 补偿，以及历史对象版本和未完成 multipart 的完整盘点。
6. 上传排队长尾及短测吞吐下降的受控性能归因；开发库 2 条历史 test 桶记录的来源与后续处置。

这些不能因单机回归通过而标为已解决；前四项的环境验收也不等同于已确认代码 bug。
