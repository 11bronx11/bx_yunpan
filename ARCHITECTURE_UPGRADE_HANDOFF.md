# BX YunPan 架构升级施工交接

> 交接日期：2026-09-05
>
> 唯一施工依据：`ARCHITECTURE_UPGRADE_FINAL_EXECUTION_PLAN.md`
>
> 目标设计：`ARCHITECTURE_UPGRADE_FINAL.md`

## 1. 当前结论

本轮已经完成计划内架构基础的大部分源码施工，并把单机运行态推进到 S1-S4 的组合形态。当前不是 P0-P9 全部完成：P0.2/P0.3/P0.4/P1/P2/P3/P4 仍为 `[~] 进行中`（源码和部分单机故障窗口已验证）；P5.2/P6.1-P6.2/P7.1/P8.5 已达到代码、接口、迁移或单机级门禁；P9 仍为 `[~] 环境门禁`。原因不是主要入口缺失，而是各工作包定义的完整兼容窗口、故障矩阵、端到端业务剧本以及 staging/生产 HA 验收尚未全部闭环。

| 工作包 | 当前事实 | 尚缺的完成证据 |
|---|---|---|
| P0.1 | `[x]` 基线已保存 | 无 |
| P0.2/P0.3 | typed event、独立 delivery、routing、消费幂等、聚合版本和主要领域事件已落地 | 旧 Relay/V2 flag 全兼容窗口、全状态写入口审计、contract 收缩 |
| P0.4 | `storage_operations`、lease fencing、recovery 和 MinIO 外部副作用收口已落地；本机 MinIO down/readiness 失败/恢复窗口已通过 | 完整真实 MinIO 故障点、接管和长期收敛矩阵 |
| P1 | 双 API、逐副本 readiness/instance ID、Nginx 转发和摘除演练已通过 | 跨副本完整注册/上传/下载/分享/AI 业务剧本及生产 Gateway 主动摘除 |
| P2 | 四个独立 Worker 容器、队列所有权、metrics 和稳定 Task ID 恢复测试已通过；本机 Relay 停止/恢复与双 delivery 发布窗口已通过 | Broker 全丢后的全部中间态恢复、多 Relay/Worker 故障矩阵和 DLQ 回放 |
| P3 | `ai-query`/`ai-indexer` 容器拆分、版本化索引、lease fencing 和 core-api Reprocess ownership 已落地；本机 query/indexer 停止隔离窗口已通过 | Provider 故障、indexer 接管、旧 active 保留和 Citation 端到端矩阵 |
| P4 | Redis Broker/Coordination/Cache 三职责运行、Embedding L1/L2、owner epoch 和 Cache degraded readiness 已落地；三角色分别停止/恢复窗口已通过 | Cache/Coordination/Broker 的完整失效消费、防击穿和 HA 故障演练 |
| P5.2 | `[x]` 代码/单机配置适配 | `POSTGRES_PRIMARY_DSN` writer 入口、迁移与配置门禁已验证；staging failover/PITR/failback 仍缺 |
| P6.1-P6.2 | `[x]` 代码/单机 profile | `000017`/`000019` 投影表、index version、ACL epoch、PostgreSQL fallback、独立 `search-indexer` 已验证；ES/Qdrant 重建、shadow、切换仍缺 |
| P7.1 | `[x]` 接口/单机测试 | Kafka producer boundary 与 Kafka-only delivery 状态机已验证；broker 故障、DLQ、watermark 重放仍缺 |
| P8.5 | `[x]` 迁移 | `000019` workflow/Saga 持久模型、状态迁移和补偿 API 已验证；远程服务 canary/故障注入仍缺 |
| P9 | `[~]` 环境门禁 | 已提供 HA/search/P9 三组 staging 前置检查脚本；生产 HA、Gateway、物理分库仍需专用环境 |

## 2. 已落地实现

- 迁移已扩展到 `000019`：独立 Outbox delivery、聚合版本、storage operation、lease token、recovery、AI document version、owner content/ACL epoch、search index version 和 workflow metadata。
- Outbox 使用 typed envelope、显式 routing、每 sink delivery、至少一次投递、稳定 Task ID 和 `event_consumptions` 幂等接口。新增测试覆盖 Redis 已接收任务但 PostgreSQL delivery 状态提交失败后，重启重复投递由 Task ID 吸收。
- 上传、canonical object、GC 等 MinIO 副作用通过持久 operation 和 reconciler 管理；claim 使用 `FOR UPDATE SKIP LOCKED`，最终状态更新受 lease token fencing。
- API 支持多副本；健康检查区分硬依赖和 advisory。PostgreSQL、MinIO 为 readiness 硬依赖，Cache Redis 失败只报告 degraded，不阻断核心文件 API。
- 独立运行角色为 `outbox-relay`、`object-worker`、`media-worker`、`maintenance-worker`、`ai-query`、`ai-indexer`；兼容 `worker`/`aisvc` 仍保留为回滚入口。
- `deploy/up.sh` 支持 `--distributed-workers`、`--ai-split`、`--redis-split`、`--api-replicas N`、`--aisvc-replicas N`，并负责清理互斥的旧运行角色。
- Redis 客户端按 Broker、Coordination、Cache 配置隔离；缓存配置为 `CACHE_ENABLED`、`CACHE_TTL`、`CACHE_L1_MAX_ITEMS`。当前只应把缓存视为可丢失派生数据，权限、配额、上传最终态和引用计数仍以 PostgreSQL 为准。
- Search projection 有独立 index version 生命周期、content/file checkpoint、ACL epoch 和 tombstone 版本保护；Kafka dispatcher 只拥有 Kafka delivery，Asynq 与 Kafka 状态互不覆盖。

## 3. 当前运行态

`./deploy/status.sh` 于最终收口时通过，当前 Compose 运行态为：

- `api`: 2 个副本，均 healthy/readiness OK；
- `ai-query`: 2 个副本，均 `grpc.health.v1: SERVING`；
- `ai-indexer`: 1 个，AI 队列 Worker 正常；
- `outbox-relay`、`object-worker`、`media-worker`、`maintenance-worker`: 各 1 个，metrics 探针正常；
- `redis-broker`、`redis-coordination`、`redis-cache`: 各 1 个且 healthy；兼容 `redis` 仍运行；
- PostgreSQL、MinIO、etcd、web 均运行，Web/API/Storage 探针为 HTTP 200；
- combined `worker` 和 combined `aisvc` 当前未运行，避免重复消费。

这是单机开发环境的功能与隔离证据，不是生产容量或 HA 证据。当前 search-indexer 仅写 PostgreSQL projection fallback，ES/Qdrant 和 Kafka 默认未启用。

## 4. 最终门禁和证据

最终源码门禁证据目录：`artifacts/architecture-upgrade/20260905T-final-closeout/`
本次依赖、镜像刷新和运行复验：`artifacts/architecture-upgrade/20260905T064353Z-dependency-image-refresh/`
本轮 P5-P9 代码/迁移/运行证据：`artifacts/architecture-upgrade/20260905T-p5-p6-p7-p9/`
本轮 search/Kafka/workflow focused 证据：`artifacts/architecture-upgrade/20260905T-p6-p9-followup/`
本轮 VPN 后依赖/镜像刷新证据：`artifacts/architecture-upgrade/20260905T091148Z-vpn-refresh/`
本轮 P0.4/P2/P3/P4 故障矩阵证据：`artifacts/architecture-upgrade/20260905T093948Z-fault-matrix/`

| 检查 | 结果 |
|---|---|
| Go 格式 | `gofmt -l` 无输出 |
| 后端测试 | 干净环境全量通过；含 PostgreSQL focused tests 时 167 项通过 |
| Go 静态检查 | `go vet ./...` 通过 |
| 部署脚本 | `bash -n deploy/*.sh` 通过 |
| Compose | app + distributed + ai-split + redis-split 全 profile 渲染通过 |
| 配置漂移 | `./deploy/config-check.sh` 通过 |
| 前端测试 | 14 suites / 36 tests 通过 |
| 前端 lint | 本地 ESLint 等价命令零错误通过 |
| 前端生产构建 | 编译通过 |
| 运行探针 | `./deploy/status.sh` 全部通过 |
| 差异检查 | `git diff --check` 通过 |

`npm run lint` 在本机被命令包装层错误地扩展到已忽略的 `web/build` 压缩产物，原始命令因此报错；按施工计划 7.1 的允许方式执行 `./node_modules/.bin/eslint src --ext .js --ignore-pattern build --max-warnings=0` 后通过。两份输出均保存在最终证据目录。

其他关键证据：

- `artifacts/architecture-upgrade/20260905T-final-verification/`：迁移、真实 PostgreSQL focused tests、双 API 和 AI split 早期验证；
- `artifacts/architecture-upgrade/20260905T045336Z-api-replica-recheck/`：API 副本摘除与恢复；
- `artifacts/architecture-upgrade/20260905T050456Z-role-metrics-recheck/`：四个 Worker 的队列所有权和 metrics；
- `artifacts/architecture-upgrade/20260905T050857Z-ai-split-recheck/`：query health、indexer 队列和 metrics；
- `artifacts/architecture-upgrade/20260905T022640Z-p0-risk-fixes/`：recovery、lease、并发和真实 PostgreSQL 风险收口。
- `artifacts/architecture-upgrade/20260905T064353Z-dependency-image-refresh/`：Go/npm 依赖下载、最新本地镜像 ID、容器二进制 hash 对照、迁移和全量门禁。

## 5. 镜像和依赖边界

本次已完成依赖下载和最新本地镜像刷新。Go 模块通过临时 `GOPROXY=https://goproxy.cn,direct` 下载，Web 依赖通过临时 `https://registry.npmmirror.com` 安装；仓库文件中的默认源没有修改。

官方 Docker Hub 在本机仍因 TLS handshake timeout 不可用，DaoCloud、1ms 等公开 registry 访问分别出现 401 或超时。因此没有继续进行外部基础层 pull，而是使用本机已验证运行的 Alpine/Nginx 层，把当前工作树串行编译出的静态 Go 二进制和 React build 重新封装为本地镜像。镜像 ID、构建 hash 和命令输出见 `artifacts/architecture-upgrade/20260905T064353Z-dependency-image-refresh/`。

已执行并通过：

```bash
./deploy/up.sh --no-build --distributed-workers --ai-split --redis-split \
  --api-replicas 2 --aisvc-replicas 2
./deploy/status.sh
./deploy/config-check.sh
```

当前容器内 `/app/service` hash 与主机构建目标逐一匹配；迁移为 `19|false`，`migrate` exit 0。单机运行态证明当前源码已进入本地运行镜像，不等同生产 HA 或容量证据。

## 6. 后续施工顺序

本轮补充：运行数据库已迁移到 `19|false`，当前源码已重新封装进本地后端镜像；新增 `deploy/staging-p9-check.sh` 通过前置门禁后，才能进入专用 staging 的 HA、Kafka、ES/Qdrant、Gateway 和物理分库演练。
P9 顺序与回滚约束记录在 `deploy/P9_STAGING_RUNBOOK.md`；当前环境没有专用 endpoint，因此不能把 P9 标记为完成。

### 2026-09-05 接手续施工复验

- 新增证据目录：`artifacts/architecture-upgrade/20260905T1015Z-handoff-continuation/`。
- 使用当前运行 PostgreSQL 的真实 DSN，后端 `go test ./... -count=1` 为 172 项通过；`go vet ./...`、`gofmt`、`git diff --check`、`bash -n deploy/*.sh` 和 Compose 全 profile 渲染均通过。
- 前端 14 suites/36 tests、直接 ESLint 和生产构建均通过；`./deploy/status.sh`、`./deploy/config-check.sh` 通过，迁移为 `19|false`，Outbox pending/failed 为 `0/0`。
- 配置默认值单测已与宿主部署环境隔离，避免 `deploy/.env` 的可选 reranker 配置污染默认值断言；配置测试错误信息不再打印完整 `Config`、DSN、Redis 密码或 `DASHSCOPE_API_KEY`。
- `staging-ha-check.sh`、`staging-search-check.sh`、`staging-p9-check.sh` 在当前环境均按设计以退出码 `2` 拦截缺失的专用 staging endpoint；这不是代码失败，也不构成 P5-P9 完成证据。

### 2026-09-05 单机核心热路径闭环

- 新增证据：`artifacts/architecture-upgrade/20260905T1015Z-handoff-continuation/core-hotpath-summary.md`。
- 真实 HTTP 闭环通过：注册/登录、目录、单分片预签名上传、MinIO 写入、异步完成、下载内容逐字节校验、分享解析/访问/撤销、文件和目录删除、退出登录。
- 同一 `Idempotency-Key` 重复创建只得到同一 upload session；停止一个 API 副本时 `/readyz` 10/10 返回 200，恢复后 healthy，`status.sh` 通过。
- 停止 Cache Redis 时 `/readyz` 保持 `status=ok`、仅 `redis_cache=degraded`，登录和根目录读取仍为 HTTP 200；恢复后 healthy。
- PostgreSQL 权威状态：迁移 `19|false`，Outbox event/delivery pending/failed `0/0`，storage operation 非终态 `0`，活动上传 `0`。
- 本轮未调用外部 DashScope provider，避免把外部计费依赖引入单机核心验证；AI 健康、拆分角色和 fallback 仍以已有单测/运行探针为边界。

### 2026-09-05 本地 staging 核心故障闭环

- 新增可重复入口：`deploy/local-staging-check.sh`。脚本只暂停/恢复现有 Compose 服务，不删除 volume；Go 测试使用按 PID 创建的临时 PostgreSQL 数据库，结束后自动删除。
- API 副本、Cache Redis、Broker Redis + Outbox Relay、MinIO 故障窗口均通过；恢复后 Web/API/Storage 探针和全部容器回到 healthy。
- Broker 恢复场景验证 PostgreSQL Outbox 在 broker 不可用时保持 pending，恢复 broker/relay 后同一事件收敛为 published；Kafka producer 失败/恢复状态机测试已加入 `backend/internal/outbox/kafka_dispatcher_test.go`。
- 临时库迁移 `19|false`，`go test ./... -count=1` 全部通过；主库最终 `outbox_pending=0`、`outbox_delivery_pending=0`、`storage_non_terminal=0`。
- 运行产物：`artifacts/architecture-upgrade/20260905T141016Z-local-staging/`。本节关闭本地核心模拟门槛，不改变专用 staging 的生产 HA/PITR、Kafka broker、ES/Qdrant、Gateway 和物理分库状态。

1. 在本机已完成可逆的 Cache/Broker/Coordination、Relay、AI split、MinIO 故障窗口；下一步补齐 Broker 全丢后的完整中间态重建、多 Relay/Worker 接管、AI active version 保留和权限 Citation 端到端剧本。
2. 只有对应退出门槛齐全后，才能把 P0.2-P4 的单项改为 `[x]` 或执行 contract migration。
3. 先运行 `deploy/staging-p9-check.sh` 通过前置门禁，再把 P5-P9 转到专用 staging/生产环境：PostgreSQL/Redis/MinIO HA、备份/PITR/failover、ES/Qdrant、Kafka、Saga、生产 Gateway、物理分库和分片不能用当前单机 Compose 代替。

## 7. 接手约束

- 工作树包含大量未提交源码、迁移、部署和文档改动，禁止 reset、checkout 或清理用户改动。
- 不删除 PostgreSQL 数据、Docker volume、Redis 数据、MinIO 对象、缓存或现有证据目录。
- 不打印、记录或提交 `DASHSCOPE_API_KEY`。
- 所有构建保持串行和低内存限制；当前机器不再尝试并行冷构建。
- PostgreSQL 始终是权限和业务事实源；ES/Qdrant、Redis、Kafka 和 Asynq 只能是可重建派生平面。
- “代码存在”“宿主测试通过”“容器探针通过”“生产 HA 完成”是四个不同结论，交接和验收时不得混写。
