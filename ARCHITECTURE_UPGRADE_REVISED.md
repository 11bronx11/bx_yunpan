# BX YunPan 架构升级计划（平衡修订版）

## 1. 修订目标

本计划保留原方案中有价值的分布式和微服务方向，但修正以下问题：

- 不再把服务数量当成架构成果；
- 不再假设 Schema 隔离能够提供跨进程事务；
- 不再一次性迁移所有服务和基础设施；
- 不再把 ES、Qdrant、Kafka 当成业务事实源；
- 不再把单机 Compose 当成生产高可用环境；
- 每个阶段必须有数据边界、事件契约、切换方式和回滚方式。

目标不是维持现状，而是沿着“事务核心 → 分布式运行时 → 派生读模型 → 事件总线 → 写服务拆分”的路径，最终形成真正可演进的微服务架构。

---

## 2. 保留、修正和后置

### 2.1 保留

1. `aisvc` 独立部署、gRPC 通信和 etcd 服务发现；
2. API Gateway 的限流、熔断、灰度和 Trace 传播；
3. PostgreSQL Transactional Outbox；
4. Worker 按队列独立扩容；
5. Kafka 作为长期领域事件总线；
6. Elasticsearch 作为全文读模型；
7. Qdrant 作为向量读模型；
8. PostgreSQL HA、Redis HA 和对象存储 HA；
9. CQRS，但只用于派生读模型，不替代事务写模型。

### 2.2 修正

| 原方案 | 修订后的含义 |
|---|---|
| 五个服务同时拆分 | 先建立五个服务契约，再按领域逐个切换写流量 |
| Schema 隔离保留事务 | Schema 只作为过渡；跨服务操作使用命令、事件和 Saga |
| Kafka 替换 Asynq | Kafka 负责领域事件，Asynq 继续负责可重试任务 |
| Drive 拥有所有对象数据 | 逻辑文件、物理对象、上传会话分别定义 owner |
| ES/Qdrant 直接提供搜索结果 | 索引提供候选，权限和状态由权威服务确认 |
| 自研 Gateway 800 行即可生产 | 自研版本用于学习和定制；生产保留 Envoy/Kong/Nginx 替代路径 |
| etcd + Consul 互相补充 | etcd 负责发现/DCS，Consul 只在配置中心需求成立时引入 |

### 2.3 后置

- 物理分库；
- Redis Cluster 分片；
- Kafka 多节点生产集群；
- Qdrant 多分片多副本；
- Elasticsearch 多节点；
- 全量写服务拆分。

后置不代表取消，而是要求先完成协议、故障和迁移验证。

---

## 3. 目标架构

### 3.1 最终逻辑形态

```text
Browser
   |
Nginx / Gateway
   |
   +--> identity-svc
   +--> drive-svc
   +--> upload-svc
   +--> sharing-svc
   +--> ai-svc
   |
   +--> workflow/orchestrator（跨服务流程）

identity-svc：用户、认证、refresh session、账户配额
drive-svc：目录、逻辑文件条目、权限
object-svc：物理对象、引用计数、GC、MinIO 生命周期
upload-svc：上传会话、分片、校验流程
sharing-svc：分享、授权、导入意图
ai-svc：抽取、Embedding、问答、AI 文档和 AI 任务
search-indexer：ES/Qdrant 派生索引
worker：媒体、对象校验、维护任务
```

`object-svc` 是对原方案的必要补充。物理对象和逻辑文件条目不是同一个聚合，不能简单全部归 `drive-svc`。

### 3.2 数据层

```text
业务服务
   |
   +--> PostgreSQL（阶段 1：同一集群、分 Schema）
   |       users / folders / file_entries / file_objects ...
   |
   +--> Outbox
           |
           +--> Asynq：任务执行
           +--> Kafka Relay：长期领域事件（阶段 4）

MinIO：文件字节和媒体变体
Redis：限流、锁、Asynq Broker（按阶段升级 HA）
ES/Qdrant：可删除、可重建的派生索引
```

阶段 1 的“同一 PostgreSQL”只是降低迁移成本，不允许服务直接修改别人的表。跨服务必须调用命令或消费事件。

### 3.3 一致性模型

- 同一聚合内部：本地事务，强一致；
- 跨聚合、跨服务：Saga，最终一致；
- 搜索索引和 AI 索引：异步派生，可重建；
- 文件字节和元数据：通过状态机、补偿任务和孤儿对象扫描保持可恢复；
- 不引入 XA/TCC 作为默认方案。

---

## 4. 服务边界和数据所有权

| 服务 | 首要所有权 | 对外能力 | 禁止事项 |
|---|---|---|---|
| identity-svc | users、refresh_sessions、账户配额/配额账本 | 登录、刷新、账户状态、配额命令 | 直接创建文件条目 |
| drive-svc | folders、file_entries、ACL/逻辑文件状态 | 目录、改名、移动、删除、文件元数据 | 直接操作 MinIO 或改对象引用 |
| object-svc | file_objects、object_variants、引用计数、GC 状态 | 注册对象、增加/释放引用、对象回收 | 直接决定用户权限 |
| upload-svc | upload_sessions、upload_parts、staging 状态 | 创建会话、预签名、分片确认、完成校验 | 直接跨服务扣配额和创建逻辑文件 |
| sharing-svc | shares、share_imports、分享授权状态 | 创建分享、解析分享、提交导入意图 | 直接跨服务修改 Drive/Object 表 |
| ai-svc | ai_documents、ai_chunks、AI 任务状态 | 抽取、索引、搜索、问答、重处理 | 直接改变文件权限和配额 |
| search-indexer | ES 索引、Qdrant collection | 索引构建、重建、删除 | 作为权限事实源 |
| workflow/orchestrator | Saga 状态、步骤、补偿记录 | 编排注册、上传完成、分享导入等流程 | 保存领域主数据 |

所有服务都可以在同一 PostgreSQL 集群中运行，但只有 owner 可以写自己的表；外键跨 Schema 仅作为过渡约束，不能作为服务间调用协议。

---

## 5. 核心跨服务流程

### 5.1 上传完成

```text
Browser -> MinIO：分片直传
Browser -> upload-svc：complete
upload-svc：校验对象并记录 upload.completed
workflow：启动 FinalizeUpload Saga
  1. identity-svc：ReserveQuota
  2. object-svc：CreateOrGetObject（按 SHA256 幂等）
  3. drive-svc：CreateFileEntry（按幂等键）
  4. identity-svc：CommitQuota
  5. object-svc：AddReference
  6. 发布 file.created / object.ready
失败：释放配额、撤销临时对象引用、进入补偿队列
```

迁移初期仍可由现有 `core-api` 在本地事务中完成这条流程；只有 Saga 版本经过重复、超时和回滚测试后，才切换到跨服务流程。

### 5.2 分享导入

```text
sharing-svc：校验 share token，写入 import intent
workflow：启动 ImportShare Saga
  1. drive-svc：确认目标目录和文件名
  2. identity-svc：ReserveQuota
  3. drive-svc：CreateFileEntry
  4. object-svc：AddReference
  5. identity-svc：CommitQuota
  6. sharing-svc：标记 share_import completed
```

每个步骤使用同一个 `import_id` 和幂等键。重复请求返回已有结果，不重新增加配额或引用计数。

### 5.3 删除和 GC

```text
drive-svc：逻辑删除 file_entry
identity-svc：减少逻辑配额
object-svc：ReleaseReference
object-svc：引用为零时写入 GC 状态
worker：删除 MinIO 对象和派生变体
object-svc：提交 deleted 状态
search-indexer：消费 tombstone
```

删除流程不要求所有系统同步完成，但必须保证：未确认删除的对象不会被物理清理，已删除文件不会继续出现在搜索结果。

### 5.4 注册

第一阶段保留现有注册事务。真正拆分 Identity 后：

```text
identity-svc：创建用户和 refresh session
发布 account.created
drive-svc：幂等创建根目录
identity-svc：完成账户 provisioning 状态
```

如果产品要求注册请求立即返回可用根目录，则由 workflow 同步等待 Drive 命令完成；否则返回 provisioning 状态。

---

## 6. 事件和任务

### 6.1 统一事件信封

```json
{
  "event_id": "stable-id",
  "event_type": "file.created.v1",
  "aggregate_type": "file_entry",
  "aggregate_id": "uuid",
  "aggregate_version": 3,
  "occurred_at": "timestamp",
  "producer": "drive-svc",
  "trace_id": "optional",
  "payload": {}
}
```

事件必须支持向后兼容、重放、乱序检测和重复消费。消费者使用 `event_id` 或 aggregate version 实现幂等。

### 6.2 事件类型

保留当前事件：

- `object.verify_requested.v1`；
- `object.ready.v1`；
- `object.gc_requested.v1`；
- `file.created.v1`；
- `share.imported.v1`；
- AI 索引和重处理事件。

补充搜索和 Saga 所需事件：

- `upload.completed.v1`；
- `file.updated.v1`；
- `file.deleted.v1`；
- `folder.updated.v1`；
- `share.permission_changed.v1`；
- `quota.reserved.v1`、`quota.committed.v1`、`quota.released.v1`；
- `saga.step_completed.v1`、`saga.failed.v1`。

### 6.3 Asynq 和 Kafka 的分工

第一阶段继续使用 Asynq：

- 对象校验、缩略图、AI 处理、GC、过期清理使用 Asynq；
- 任务拥有 timeout、retry、queue、retention 和用户可见状态；
- Worker 和 aisvc 继续按队列独立扩容。

Kafka 只承载需要长期保留或被多个独立服务消费的领域事件：

```text
各服务 Outbox -> Kafka Relay -> Kafka topic
                             |-> search-indexer
                             |-> audit/projector
                             |-> analytics/other consumers
```

Kafka 引入后仍保留 Outbox。必须定义 producer ACK、partition key、consumer group、offset、DLQ、重放、schema 兼容和旧 Asynq 任务排空方案。

---

## 7. 分阶段实施

### 阶段 0：架构契约和事务保护

产物：

- 服务所有权矩阵；
- 聚合和不变量清单；
- 事件信封和版本规则；
- Saga 状态机模板；
- API/Proto 版本策略；
- 重复请求、重复消费、超时和补偿测试。

此阶段不改变现有部署拓扑。目标是先让拆分规则可检查、可测试。

### 阶段 1：分布式运行时和 Gateway

保留现有 REST API，完成：

- `core-api` 无本地状态并支持多副本；
- Worker 按队列独立扩容；
- `aisvc` 保持 gRPC + etcd；
- Gateway 代理现有 HTTP API；
- 限流、熔断、灰度和 Trace 传播集中到入口；
- 上传和下载继续走预签名 URL/MinIO 数据面。

这一阶段不强迫所有业务接口改成 gRPC，也不拆跨事务写模型。

### 阶段 2：服务骨架和逻辑 Schema

创建服务进程、API/Proto 和独立部署单元：

- identity-svc；
- drive-svc；
- upload-svc；
- sharing-svc；
- object-svc；
- aisvc；
- workflow/orchestrator。

服务可以共享一个 PostgreSQL 集群，但只允许 owner 写自己的 Schema。跨领域写操作暂时继续由 `core-api` 的事务用例执行，并通过 Anti-Corruption Layer 暴露给新服务，避免半成品 Saga 直接接管生产流量。

完成标准：每个服务都有独立健康检查、契约测试、日志和 trace；没有服务直接修改其他服务的 Model。

### 阶段 3：Outbox 事件供应链

完成：

- 事件生产者从 Handler 移到 Application Use Case；
- Outbox Relay、Inbox/去重表和事件重放工具；
- `file.updated`、`file.deleted`、`folder.updated` 等索引事件；
- Saga 步骤状态、超时和补偿记录；
- Asynq 任务和领域事件的明确映射。

完成标准：Redis、Worker、消费者任意时刻重启，事实数据不丢；事件重复或乱序不会重复扣配额、增加引用或覆盖新状态。

### 阶段 4：搜索读模型

实现独立 `search-indexer`：

- 从 PostgreSQL 快照开始构建；
- 从对应事件版本继续消费；
- ES 保存全文字段和逻辑文件信息；
- Qdrant 保存向量、object/file 映射和必要过滤字段；
- 事件按 aggregate version 防止旧状态覆盖新状态；
- 删除使用 tombstone；
- 先影子查询，再按开关切换；
- PostgreSQL 保留为权限确认、降级和全量重建来源。

完成标准：索引删除后可重建；目录移动、改名、删除和权限变化均有测试；索引故障不影响核心文件操作。

### 阶段 5：Kafka 事件总线

前置条件：

- 阶段 3 的事件版本、重放和幂等已经稳定；
- 至少有两个长期消费组；
- 已完成单节点功能验证和多节点故障演练；
- 已定义 topic、partition、保留期、DLQ 和恢复流程。

实施：

1. 部署 Outbox Relay；
2. 先让非关键 projector 消费 Kafka；
3. 与 Asynq/旧 projector 结果比对；
4. 逐个迁移长期事件消费者；
5. Asynq 继续承载任务型工作。

Kafka 故障时，业务写入继续成功，Outbox 积压；Kafka 恢复后从未确认位置继续投递。

### 阶段 6：逐个切换写服务

推荐顺序：

1. Identity 的登录、刷新和账户查询；
2. Upload 的会话、预签名和 staging 管理；
3. Sharing 的分享创建、解析和导入意图；
4. Object 的对象生命周期和 GC；
5. Drive 的文件条目、目录和 ACL。

每次只切一个边界：

- 旧路径和新路径双读或影子执行；
- 新服务先接只读流量；
- 再切幂等写命令；
- 最后关闭旧实现；
- 任何一步都可以通过 feature flag 回滚。

Drive 最后拆，因为它与目录、对象、配额、分享和搜索关联最多。

### 阶段 7：物理数据拆分和生产 HA

只有阶段 6 的服务边界稳定后，才迁移到物理数据库：

- 每个服务拥有独立数据库账号和迁移；
- 跨服务不再使用外键和跨库 SQL；
- 通过 API、事件和 Saga 维持一致性；
- 先迁移只读或派生数据，再迁移写模型；
- 保留回填、双读和旧库只读窗口。

基础设施目标：

- PostgreSQL：Patroni + 三节点 DCS + HAProxy/托管数据库；
- Redis：先 Sentinel/托管 Redis，确认需要分片后再 Cluster；
- MinIO：分布式纠删码或外部对象存储；
- Qdrant：多节点、多分片、多副本；
- Elasticsearch：至少定义副本、快照、重建和故障降级；
- Kafka：多节点和明确副本策略；
- etcd：三节点，服务发现和 Patroni DCS 共用时必须隔离权限；
- Consul：仅用于明确的动态配置需求，和 etcd 的职责分离。

开发环境可使用单节点 Compose profile，但不宣称生产 HA。

---

## 8. Gateway 设计

Gateway 负责：

- TLS 和可信边缘转发；
- JWT 验证、请求 ID 和 Trace 传播；
- 用户级和接口级限流；
- AI 上游熔断、超时和降级；
- 灰度路由和稳定用户分流；
- 统一错误边界。

第一阶段继续代理现有 HTTP API。后续服务拥有稳定 Proto 后，再逐个增加 gRPC 路由。

灰度 Header 只能由可信边缘注入，公网入口必须剥离用户伪造的 Header。服务间通信在跨主机部署时必须启用 TLS 或 mTLS，并定义服务身份和授权。

自研 Gateway 可以保留为项目亮点，但必须限制范围并保留成熟代理替代路径。

---

## 9. 高可用和配置策略

### PostgreSQL

先保证备份、PITR、故障转移、连接池重建和迁移兼容，再做读写分离。

读副本启用后：

- 事务内读固定到 Primary；
- 写后立即读使用 Primary 或粘滞路由；
- 明确复制延迟和 failover 期间的错误语义；
- Patroni 的 DCS 必须是有 quorum 的 etcd/Consul 集群。

### Redis

任务、限流和锁共享 Redis 时，先验证 Sentinel/托管方案。迁移 Cluster 前必须验证 go-redis、Asynq、Lua 脚本、hash slot、DB 语义和节点故障后的任务恢复。

### 配置中心

默认使用环境配置和版本化配置文件。若引入 Consul：

- 配置必须有 schema 和版本；
- 无效配置拒绝生效；
- watch 中断时保留最后有效配置；
- 配置变更必须审计并可回滚；
- Secret 不直接作为普通 KV 暴露。

---

## 10. 验收标准

### 服务化

- 每个服务有明确 owner、API/Proto、健康检查和独立日志；
- 服务不能直接修改其他服务的表；
- 新旧路径可以同时运行；
- 每次只切换一个服务边界。

### 一致性

- 重复上传不会重复创建文件、扣配额或增加引用；
- 重复分享导入返回已有结果；
- Saga 中断后可以继续或补偿；
- Outbox、Kafka、Asynq 重复投递不会破坏状态；
- 对象字节、元数据和索引之间存在可检测、可修复的不一致状态。

### 搜索和 AI

- AI 故障不阻塞核心文件操作；
- ES/Qdrant 可以从事实库重建；
- 删除、移动、改名和权限变化最终反映到索引；
- 搜索结果不能绕过权威服务的权限和状态确认。

### 高可用

- PostgreSQL、Redis、MinIO、Kafka、搜索集群分别有故障演练；
- 单个服务、消费者或索引节点故障不会扩大为全站故障；
- 恢复时间和数据丢失结论只对实际演练过的拓扑有效。

---

## 11. 最终原则

原计划的技术广度可以保留，但必须改成分层演进：

```text
模块化单体
   -> 分布式运行时
   -> 服务契约和事件供应链
   -> ES/Qdrant 派生读模型
   -> Kafka 长期事件流
   -> 逐个切换写服务
   -> 物理分库和生产 HA
```

这样既保留了微服务、分布式、CQRS、Kafka、搜索集群和 HA 的架构升级价值，又不会因为跨服务事务、权限一致性和迁移回滚没有设计而把系统改坏。

