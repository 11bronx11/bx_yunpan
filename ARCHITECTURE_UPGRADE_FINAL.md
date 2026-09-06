# BX YunPan 最终架构升级方案

## 1. 目标与原则

当前系统是“模块化单体 + 独立 aisvc”，主要问题不是模块数量少，而是 API 仍然偏向单节点运行，异步、缓存、数据库和搜索能力还没有形成可独立扩展的运行平面。

最终目标是：

> 保留强事务聚合，让计算、任务、搜索和 AI 按负载独立扩展；只有真正具备独立生命周期和扩展收益的领域，才拆成独立服务。

基本原则：

- 逻辑边界和物理进程不强行一一对应；
- 同一聚合内部保持本地事务，跨聚合使用事件、命令和 Saga；
- PostgreSQL 是业务事实源，ES/Qdrant 是可删除、可重建的派生读模型；
- 异步采用至少一次投递，所有任务和消费者必须幂等；
- 先实现无状态多节点，再做物理分库和基础设施集群化；
- 不以服务数量、容器数量或未经验证的性能数字作为架构成果。

## 2. 目标运行形态

```text
Browser
   |
   v
Gateway / Load Balancer（多副本）
   |
   +-- core-api（无状态，多副本）
   |     +-- account：identity、refresh session、quota
   |     +-- content：drive、sharing
   |     +-- storage：upload、object metadata、GC 状态
   |     +-- 文件和媒体查询 API
   |
   +-- ai-query / aisvc（gRPC，多副本）
   |
   +-- workflow（初期可在 core-api 内，跨服务后独立）

Outbox Relay
   +-- Asynq -> object-worker / media-worker / ai-indexer / maintenance-worker
   +-- Kafka  -> search-indexer / audit / 其他长期消费者

PostgreSQL HA（业务事实源）
Redis（Broker、协调、缓存按职责隔离）
MinIO HA（文件字节和媒体变体）
ES + Qdrant（可重建搜索和向量读模型）
```

这里的 `core-api` 仍然可以是一个部署单元。它不是退回单体，而是把事务紧密、需要一起发布的领域合理聚合，同时支持多个无状态副本。

## 3. 服务和聚合边界

### 默认聚合

初期保持以下事务核心在 `core-api`：

```text
account = identity + quota
content = drive + sharing
storage = upload + object metadata + GC 状态
```

这样可以继续保护当前已有的关键本地事务：上传会同时涉及配额、文件条目、物理对象和 Outbox；分享导入会同时涉及配额、文件条目、对象引用和导入记录；注册会创建用户、会话和根目录。

### 优先独立的能力

这些部分已经有明确的独立扩展理由：

- `aisvc`：在线搜索、问答和模型调用；
- `ai-indexer`：抽取、切块、Embedding、摘要和索引构建；
- `media-worker`：缩略图和媒体变体；
- `object-worker`：对象校验、引用回收和 GC；
- `maintenance-worker`：过期清理和维护任务；
- `search-indexer`：ES/Qdrant 的构建、回填和删除；
- `gateway`：入口治理和流量分配。

### 条件式拆分

当某个领域出现独立扩容、独立发布或故障隔离需求，并且跨服务协议已经稳定时，再拆出：

- `storage-svc`：上传会话和物理对象生命周期；
- `content-svc`：目录、逻辑文件和权限；
- `account-svc`：用户、会话和配额。

`sharing` 可以继续和 `content` 聚合；`upload` 可以继续和 `storage` 聚合。没有必要为了形成五个服务而拆分。

## 4. 多节点与扩缩容

### core-api

- 所有副本无本地会话、本地文件和必须持久化的本地状态；
- JWT 校验使用统一签名配置，refresh session 存在 PostgreSQL；
- 上传状态、幂等键、锁、限流和 Outbox 使用共享基础设施；
- Gateway 根据健康检查摘除故障副本，缩容先摘流量再优雅停机；
- API 副本增加只扩展请求处理能力，不等于 MinIO 或 PostgreSQL 数据面自动扩展。

### AI

`ai-query` 和 `ai-indexer` 分开扩展：

```text
ai-query：搜索、问答、Rerank，按在线请求扩容
ai-indexer：抽取、Embedding、索引，按 ai 队列积压扩容
```

aisvc 使用 gRPC 服务发现和负载均衡；在线请求有超时、熔断和全局限流，离线任务允许重试但不能重复写入有效状态。

### Worker

按队列和资源类型独立扩容：

```text
object-worker
media-worker
ai-indexer
maintenance-worker
upload-cleaner
```

Outbox Relay 可以部署多个副本进行容灾，但同一时刻只允许持有选主锁的副本主动投递；重复投递仍必须由任务 ID 和业务幂等吸收。

## 5. 异步体系

### 组件分工

```text
业务本地事务
   -> PostgreSQL 业务数据 + Outbox（同一事务）
       -> Asynq：短期可重试执行任务
       -> Kafka：长期保留、可重放、多消费者领域事件
```

- Asynq 负责对象校验、缩略图、AI 索引、GC 和过期清理；
- Kafka 负责文件、目录、权限等领域事件，供搜索索引、审计和其他长期消费者使用；
- Workflow/Saga 只负责上传完成、分享导入、账户初始化等跨聚合流程，不替代普通任务队列。

领域事件和执行任务分开命名。事件信封至少包含 `event_id`、`event_type`、聚合 ID、聚合版本、生产者、发生时间和 trace 信息。消费者按事件 ID 或聚合版本去重，支持乱序检测、重试、DLQ 和重放。

上传完成的异步链路为：

```text
MinIO 直传
  -> upload 状态变为 verifying
  -> object.verify 任务
  -> 校验并登记 canonical object
  -> object.ready 事件
  -> media 缩略图任务 + ai.index 任务
  -> search-indexer 更新派生索引
```

故障时遵循以下语义：事务已提交但 Relay 宕机则 Outbox 继续积压；任务重复入队不重复扣配额或增加引用；Kafka 不可用不阻塞核心写入；AI 或索引失败只影响派生能力，不影响核心文件操作。

## 6. 缓存体系

缓存采用两级结构：

```text
L1：进程内 LRU
L2：独立 Redis Cache
事实源：PostgreSQL / ES / Qdrant
```

优先缓存：

- 查询 Embedding；
- 文件夹面包屑和短期文件元数据；
- 带版本的分享信息；
- 带权限范围和索引版本的短期搜索候选。

Key 必须包含模型、用户作用域、资源版本或索引 epoch，例如：

```text
embed:{model}:{dimension}:{query_hash}
folder:{owner_id}:{folder_id}:{version}
search:{scope_hash}:{query_hash}:{index_epoch}
share:{share_id}:{share_version}
```

文件创建、删除、移动、改名和权限变化通过 Outbox 事件失效相关缓存；TTL 只作为兜底。热点 Key 使用 singleflight、TTL 抖动和必要的短锁防止击穿。

以下数据不走普通缓存：配额余额、上传最终状态、对象引用计数、权限最终判断，以及没有用户作用域的搜索结果。最终 AI 答案默认不缓存，除非同时绑定权限版本、证据版本和模型版本。

Redis 按职责分离：

```text
Redis Broker：Asynq
Redis Coordination：锁、限流、Outbox 选主
Redis Cache：业务缓存
```

先采用独立实例、托管 Redis 或明确命名空间；确认客户端、Lua、Asynq 和故障恢复语义后，再迁移 Sentinel 或 Cluster。

## 7. RAG 与搜索

当前 PostgreSQL FTS + pgvector、并行召回、RRF、Reranker、Evidence 和 Citation 校验继续保留，作为稳定的第一阶段实现。

AI 已经通过 `aisvc` 独立部署，因此这部分允许比核心文件业务更积极地演进。保持 gRPC 契约不变后，内部可以替换向量库、全文引擎和缓存；AI 的索引或模型故障只能影响 AI 能力，不能阻塞上传、目录、分享和下载。

目标索引架构为：

```text
PostgreSQL：文件权限、状态、版本和 AI 事实数据
Elasticsearch：全文检索派生索引
Qdrant：向量检索派生索引
ai-indexer：构建和更新索引
ai-query：召回、RRF、Rerank、证据编排
```

`ai-query` 和 `ai-indexer` 是两个独立的扩展单元：

```text
ai-query：在线搜索、问答、Rerank、Query Embedding
ai-indexer：抽取、切块、批量 Embedding、摘要和索引更新
```

在线查询按请求量扩容，索引服务按 `ai` 队列积压和模型限流扩容。索引任务采用对象、Pipeline 和模型版本幂等，模型升级时建立新版本索引，完成后再切换别名。

索引平面和查询平面分开。ES/Qdrant 只提供候选，不能成为权限事实源。查询结果返回后，仍需依据 PostgreSQL 的所有权、删除状态、目录范围、ACL 版本和对象状态确认。

索引记录至少带有：

```text
file_id / object_id / owner_id / folder_id
file_version / pipeline_version / model_version / acl_version
tombstone
```

向量和全文存储的演进采用双写、影子查询和可回退切换：

```text
pgvector / PostgreSQL（现有基线）
        |
        +-- Qdrant：向量召回、Payload 过滤、分片和副本
        +-- Elasticsearch：全文召回、中文分析和 BM25
```

Qdrant 和 Elasticsearch 都是派生索引。迁移期间保留 pgvector/PostgreSQL fallback，先比较召回结果、权限结果和索引延迟，再通过 Feature Flag 切换读取。Qdrant 的量化、HNSW 参数、分片和副本在召回质量与故障恢复验证后逐步启用；Elasticsearch 的中文分析插件和 Mapping 作为版本化部署内容管理。

创建、改名、移动、删除和权限变化都产生可重放事件；索引更新按聚合版本幂等，删除先写 tombstone 再清理。必须支持从事实库重建、影子查询、版本切换、回填、失败重试和 PostgreSQL fallback。

RAG 查询链路保持：

```text
查询 Embedding（优先缓存）
  -> FTS + 向量并行召回
  -> RRF
  -> 候选数受控的 Reranker
  -> 权威权限确认
  -> 每文件数量受控的 Evidence
  -> Citation 白名单校验
  -> LLM
```

全文、向量和文件名召回先并行执行，再进行候选去重和 RRF；可以根据查询类型使用加权 RRF，专有名词偏向全文，自然语言问题偏向语义召回。Reranker 只处理受控数量的候选，超时或故障时降级为融合后的原顺序，并限制每个文件进入 Evidence 的数量。

AI 缓存优先放在查询侧：

```text
L1：aisvc 进程内 LRU
L2：独立 Redis Cache
```

优先缓存 Query Embedding，其次是带用户作用域、ACL 版本和索引 epoch 的短期候选。缓存键必须包含模型、维度、查询规范化结果和索引版本；文件删除、移动、改名、权限变化通过事件失效。最终答案默认不缓存，除非同时绑定权限、证据和模型版本。

即使 Qdrant Payload 或 Elasticsearch 文档包含 `owner_id`、`folder_id`、`acl_version`，最终结果仍需由 PostgreSQL 或权威内容服务确认权限和文件状态。索引更新按聚合版本幂等，删除先写 tombstone，再异步清理。

## 8. 数据库与基础设施高可用

### PostgreSQL

先将 PostgreSQL 从单节点提升为主备高可用：

```text
Patroni + 有 quorum 的 DCS
Primary + Standby
HAProxy / 托管数据库入口
PgBouncer（按需）
```

- 写入和事务内读取固定到 Primary；
- 写后立即读使用 Primary 或粘滞路由；
- 允许延迟的列表、统计和派生读模型再使用 Replica；
- 故障转移必须包含连接池重建、迁移兼容和恢复演练；
- 备份、WAL 和 PITR 是 HA 的必要组成，不以自动切主代替备份。

初期所有业务仍可在同一 PostgreSQL 集群中使用逻辑 Schema，但只能由 owner 写自己的表。服务边界稳定、Outbox/Saga/幂等和回填验证完成后，才考虑物理分库：

```text
account-db
content-db
storage-db
ai-index-db（可选）
```

分域的最终含义是按数据所有权拆分，而不是每个 Go package 单独建库：

| 数据库 | 归属数据 | 拆分边界 |
|---|---|---|
| `account-db` | 用户、Refresh Session、账户资料 | 认证可以较早独立；配额账本先留在事务核心，协议成熟后再迁移 |
| `content-db` | 目录、逻辑文件、ACL、分享和导入记录 | `drive + sharing` 聚合 |
| `storage-db` | 上传会话、分片、物理对象、变体和 GC 状态 | `upload + object` 聚合 |
| `ai-index-db` | AI 文档、切片、索引任务元数据 | 由 `ai-indexer` 管理；ES/Qdrant 保存可重建索引 |
| `workflow-db`（按需） | Saga、补偿和编排状态 | 仅在 Workflow 独立运行时引入 |

配额是跨域事务的敏感部分：上传、分享导入和删除都要改变配额与文件/对象状态，因此不能只把 `quota` 表移动到 `account-db` 就宣布完成。物理拆分后，各数据库使用本地事务和本地 Outbox，跨库操作通过幂等命令、事件和 Saga 完成。

物理分库后不再依赖跨库外键和跨库 SQL，跨服务一致性由命令、事件和 Saga 负责。

### 数据分布策略

数据分布按以下顺序演进：

```text
单主库
  -> Primary + Replica（读写分离）
  -> 按领域拆分数据库
  -> 仅在规模确实需要时按租户/用户或对象哈希水平分片
```

水平分片不是高可用的替代品。真正分片时必须先确定路由键、跨分片查询方案、迁移和再均衡方式，并把跨分片事务改为事件或 Saga；不能把当前跨表 SQL 直接复制到多个分片。

### Redis、MinIO、搜索和 Kafka

- Redis：先 Sentinel/托管 HA，再按实际分片需求上 Cluster；Broker、协调和缓存先分职责；
- MinIO：生产使用分布式纠删码或外部对象存储，API 扩容不替代对象存储扩容；
- Elasticsearch：至少定义副本、快照、重建、索引版本和故障降级，再从单节点扩展多节点；
- Qdrant：先作为可重建派生索引，规模和负载成立后再做分片、副本和量化；
- Kafka：先单节点验证事件契约、重放、DLQ 和幂等，再建设多节点副本集群；
- etcd：服务发现和 Patroni DCS 使用有 quorum 的集群，权限和用途隔离；Consul 只有在确有动态配置需求时引入。

开发 Compose 可以是单节点 profile，但不宣称生产高可用。

## 9. 入口、治理与可观测性

Gateway/Nginx 负责统一入口、JWT 边界、请求 ID、Trace、限流、AI 熔断、灰度和健康摘除。第一阶段继续代理现有 REST，不强迫所有服务立即 gRPC 化；只有服务契约稳定后再增加 gRPC 路由。

必须观测：

- API 副本健康和连接池状态；
- Outbox 积压、Asynq 队列深度、重试和 DLQ；
- Kafka consumer lag；
- AI 索引和搜索索引延迟；
- Redis 缓存命中和锁异常；
- PostgreSQL 复制延迟、主备切换和连接耗尽。

## 10. 升级顺序

```text
单体单节点
  -> 无状态 core-api 多节点
  -> Worker 按队列拆分并独立扩容
  -> aisvc 在线查询与离线索引分离
  -> Redis 缓存、Broker、协调职责分离
  -> PostgreSQL 主备高可用
  -> ES/Qdrant 派生索引和 RAG 查询平面
  -> Kafka 长期事件流
  -> 只拆真正成立的业务边界
  -> 最后再做物理分库和生产级集群扩展
```

## 11. 明确不做

- 不一次性把 identity、drive、upload、sharing 全部拆成独立进程；
- 不把 Kafka 当成 Asynq 的直接替代品；
- 不把 ES/Qdrant 当成权限或业务事实源；
- 不把所有 SELECT 无条件路由到读副本；
- 不在当前 Redis 客户端和任务模型未经验证前直接切 Redis Cluster；
- 不以单节点 Compose、固定副本数或未经演练的 RTO/SLO 数字宣称高可用；
- 不使用 XA/TCC 作为跨服务一致性的默认方案。

最终验收标准不是“拆出了多少服务”，而是：

> `core-api` 能无状态多节点运行；AI、任务、搜索和缓存能按负载独立扩展；业务事实、异步状态和派生索引之间有清晰边界；数据库、Redis、MinIO 具备可演练的故障恢复能力；任何进一步拆分都能说明独立收益和回滚路径。
