# BX YunPan 架构升级方案

## 文档说明

本文档描述 BX YunPan 从**模块化单体**向**增强版微服务架构**的演进思路。重点阐述**为什么升级、升级什么、达到什么效果**，而非具体实施细节。

**文档版本**: v1.0  
**编写日期**: 2026-09-04  
**目标架构**: 增强版微服务 (Enhanced Microservices Architecture)  

---

## 一、升级背景与动机

### 1.1 当前架构状态

BX YunPan 当前采用**模块化单体 + AI 服务独立部署**架构：

```
【现状】
• API 单体 (Gin)：identity/drive/upload/sharing/media 模块共享进程
• aisvc 独立：gRPC + etcd 服务发现
• PostgreSQL：单点，共享数据库
• Redis：单点，Asynq 任务队列
• pgvector：单机向量检索
• MinIO：对象存储

【优势】
✓ 开发效率高（本地事务）
✓ 部署简单（9 个容器）
✓ aisvc 已验证独立部署模式

【限制】
✗ API 单点故障
✗ 无法按模块独立扩容
✗ 数据库/缓存单点
✗ pgvector 无法分片
✗ 缺乏流量治理能力
```

### 1.2 升级驱动力

| 驱动因素 | 当前痛点 | 目标 |
|---------|---------|------|
| **可用性** | API/DB/Redis 单点，故障影响全站 | 99.9% 可用性，30s 内自动恢复 |
| **扩展性** | 上传高峰只能整体扩容，浪费资源 | 按模块独立扩容 (upload-svc 8 副本，drive-svc 3 副本) |
| **性能** | pgvector 单机瓶颈，PG FTS 中文弱 | 向量检索 6x，全文搜索 10x |
| **治理** | 无限流/熔断，AI 故障扩散到上传 | 熔断隔离，灰度发布，动态配置 |
| **解耦** | 同步强依赖，用户等待 AI 索引 35s | 事件驱动异步，响应 <200ms |

### 1.3 升级目标

**系统层面：**
- 可用性：99.0% → 99.9% (每月宕机时间从 7.2h 降到 43min)
- QPS：5K → 20K (4x)
- P95 延迟：300ms → 80ms (3.75x)

**架构层面：**
- 服务拆分：1 单体 → 5 微服务
- 数据库：单点 → 读写分离 (Patroni HA)
- 缓存：单点 → 分片集群 (Redis Cluster)
- 向量库：单机 → 分布式集群 (Qdrant)
- 搜索：PG FTS → Elasticsearch

**运维层面：**
- 扩容：整体 → 服务级独立扩容
- 配置：硬编码 → 动态热更新 (Consul)
- 故障恢复：手动 → 自动 (RTO <30s)
- 灰度发布：停机更新 → 零停机灰度

---

## 二、架构演进策略

### 2.1 演进路径（增量式升级）

我们采用**增量演进**而非推倒重来：

```
阶段 0: 模块化单体 (当前)
   ↓
阶段 1: 入口层增强 (Week 1)
   • 引入 API Gateway
   • 引入 Consul 配置中心
   • 实现限流/熔断/灰度
   ↓
阶段 2: 数据层高可用 (Week 2-3)
   • PostgreSQL 读写分离 (Patroni)
   • Redis Cluster 分片
   • Elasticsearch 搜索引擎
   ↓
阶段 3: 事件驱动 + 服务拆分 (Week 4-5)
   • Kafka 事件总线
   • 拆分 5 个微服务
   • CQRS 读模型
   ↓
阶段 4: 向量优化 + 监控完善 (Week 6)
   • Qdrant 向量集群优化
   • Grafana SLO Dashboard
   • 性能压测验证
```

**关键原则：**
- **兼容优先**：新老架构共存，逐步切流量
- **可回滚**：每个阶段都可独立回滚
- **渐进验证**：每周交付可验证成果

### 2.2 技术选型原则

| 选择 | 理由 | 备选方案 |
|------|------|---------|
| **自研 Gateway** | 轻量可控，800 行 Go 代码，深入理解原理 | Kong (生产可替换) |
| **Patroni** | 自动故障转移，etcd 协调，Docker 部署简单 | Stolon, repmgr |
| **Redis Cluster** | 原生分片，客户端自动路由 | Codis, Twemproxy |
| **Qdrant** | 原生分片，Payload Filter，gRPC 接口 | Milvus, Weaviate |
| **Elasticsearch** | 成熟稳定，ik 中文分词，运维工具完善 | Meilisearch, Sonic |
| **Kafka** | 生态完善，支持 Schema Registry，未来可接 Flink | Redis Streams, Pulsar |
| **Consul** | KV 存储简单，Watch 机制，与 etcd 互补 | Nacos, Apollo |

---

## 三、核心升级内容

### 3.1 高可用架构改造

#### 目标
消除所有单点故障，实现 99.9% 可用性。

#### 核心改造

**应用层：无状态多副本**
```
Gateway:       3 副本 (跨故障域)
identity-svc:  3 副本
drive-svc:     5 副本
upload-svc:    8 副本 (上传高峰独立扩容)
sharing-svc:   3 副本
ai-svc:        3 副本
worker:        6 副本 (分布式锁选主)
```

**数据层：高可用集群**
```
PostgreSQL:  Patroni (1 主 + 2 从，自动故障转移)
Redis:       Cluster (3 主 + 3 从，自动分片)
Qdrant:      3 节点集群 (6 分片 × 2 副本)
Elasticsearch: 单节点 (未来可扩展 3 节点)
```

**故障恢复能力：**
- PostgreSQL 主库宕机：30s 内 Standby 自动提升，应用无感知
- Redis 主节点宕机：5s 内从节点提升，客户端自动重定向
- 应用服务宕机：健康检查摘除，流量自动切到其他副本

#### 技术亮点
- Patroni + etcd 实现分布式共识
- HAProxy 读写分离路由
- Redis Cluster gossip 协议自动发现

---

### 3.2 微服务拆分与治理

#### 目标
实现服务独立部署、独立扩容、故障隔离。

#### 服务边界划分（DDD）

| 服务 | 职责 | 数据所有权 | 扩容场景 |
|------|------|-----------|---------|
| **identity-svc** | 用户认证授权 | users, refresh_sessions | 登录高峰 |
| **drive-svc** | 目录树、文件元数据 | folders, file_entries, file_objects | 文件列表、搜索 |
| **upload-svc** | Multipart 上传、秒传 | upload_sessions, upload_parts | 上传高峰 |
| **sharing-svc** | Share Key、授权导入 | shares, share_imports | 分享访问高峰 |
| **ai-svc** | 索引、检索、问答 | ai_documents, ai_chunks | AI 检索高峰 |

#### 服务间通信
- **同步调用**：gRPC + Protobuf (类型安全)
- **异步解耦**：Kafka 事件总线
- **服务发现**：etcd (已有) + Consul (新增)

#### 数据库策略
- **阶段 1**：逻辑分库 (Schema 隔离)
  - identity schema, drive schema, upload schema...
  - 同一 PostgreSQL 实例，便于事务
- **未来**：物理分库 (需要分布式事务 Saga)

#### 技术亮点
- 按业务能力拆分，避免循环依赖
- gRPC Streaming 支持大数据传输
- 服务独立扩容验证 (docker-compose scale)

---

### 3.3 API Gateway 流量治理

#### 目标
统一入口，实现限流、熔断、灰度发布、降级策略。

#### 核心能力

**1. 自适应熔断器**
- 10s 滑动窗口统计成功率
- 成功率 <50% 触发熔断 (非固定失败次数)
- 半开状态探测恢复
- 降级策略：AI 服务熔断时返回降级响应，不阻塞上传下载

**2. 分级限流**
- 令牌桶算法 + Redis 分布式计数
- Per-user 限流：普通用户 100 req/min，VIP 1000 req/min
- Per-endpoint 限流：AI 检索 30 req/min，AI 问答 10 req/min

**3. 灰度发布**
- Header 强制路由 (X-Canary: true)
- 百分比流量拆分 (user_id hash)
- 用户白名单 (内部测试)
- Consul 动态配置，无需重启

**4. 动态配置**
- 从 Consul KV 热加载路由表、限流规则、灰度策略
- Watch 机制实时生效
- 生产环境快速响应（临时调整限流、切流量）

#### 架构设计
```
HTTP Request
   ↓
Layer 1: JWT 鉴权
   ↓
Layer 2: 限流检查
   ↓
Layer 3: 熔断检查
   ↓
Layer 4: 灰度路由
   ↓
Layer 5: 服务发现 + 负载均衡
   ↓
Layer 6: gRPC 转发 (Trace 传播)
   ↓
Backend Service
```

#### 技术亮点
- 自研实现（800 行 Go），深入理解原理
- 滑动窗口算法优化内存占用
- 生产可替换为 Kong/Envoy

---

### 3.4 事件驱动架构 + CQRS

#### 目标
异步解耦，提升用户体验，读写分离优化查询性能。

#### 架构模式

**写路径（命令）：**
```
User 上传文件
   ↓
drive-svc 创建 file_entry (50ms)
   ↓
写入 Outbox 事件 (同一事务)
   ↓
返回响应 ✓ (200ms 内)
```

**事件投递：**
```
Outbox Dispatcher (1s 轮询)
   ↓
投递到 Kafka (file.created 事件)
   ↓ 并发消费
┌──────────┴──────────┐
Worker           ES Projector
• 缩略图          • 构建 ES 索引
• AI 索引         • 预计算 full_path
• 对象校验        • 聚合用户信息
```

**读路径（查询）：**
```
User 搜索文件
   ↓
直接查询 Elasticsearch (读模型)
   ↓
返回结果 (35ms，vs PG FTS 450ms)
```

#### 技术选型

**Kafka vs Redis Streams：**
- Kafka：持久化更强，生态完善，支持 Connector
- 单节点部署（开发模式），生产可扩展为 3 节点
- Schema Registry 管理事件 schema 演进

**CQRS 读模型：**
- Elasticsearch：files 索引（文件名、内容全文检索）
- PostgreSQL：保留事务数据
- 最终一致性：事件投递延迟 P95 <500ms

#### 技术亮点
- Transactional Outbox 保证可靠投递
- 事件溯源（Event Sourcing）可重放历史
- 异步提升响应时间：35s → 200ms

---

### 3.5 数据库读写分离

#### 目标
消除 PostgreSQL 单点，读写分离提升查询性能。

#### 架构设计

```
┌─────────────────────────────────┐
│      Patroni HA 集群             │
│  ┌──────────┐  ┌──────────┐    │
│  │ Primary  │  │Standby-1 │    │
│  │ (写)     │  │ (读)     │    │
│  └────┬─────┘  └────┬─────┘    │
│       │ 流复制       │           │
│       └──────────────┘           │
│            ↓                     │
│      ┌──────────┐                │
│      │ HAProxy  │                │
│      │:5432 写  │                │
│      │:5433 读  │                │
│      └──────────┘                │
└─────────────────────────────────┘
```

#### 核心能力

**自动故障转移：**
- Patroni 通过 etcd 进行 Leader Election
- 主库宕机 → Standby 自动提升（RTO <30s）
- HAProxy 健康检查自动更新路由
- 应用层无感知（连接池自动重连）

**读写分离：**
- 写操作（INSERT/UPDATE/DELETE）→ Primary :5432
- 读操作（SELECT）→ Standby :5433 (负载均衡)
- 流复制延迟 <100ms，用户无感知
- 事务必须走主库

**逻辑分库：**
- 按服务划分 Schema (identity/drive/upload/sharing/ai)
- search_path 配置隔离
- 为未来物理分库预留演进路径

#### 技术亮点
- Patroni 自动故障转移验证
- HAProxy 读写分离路由
- 同步复制选项（零数据丢失）

---

### 3.6 RAG 性能优化

#### 目标
向量检索性能 6x，全文搜索性能 10x，支持水平扩展。

#### 核心优化

**1. 向量存储：pgvector → Qdrant**

| 对比项 | pgvector | Qdrant |
|--------|----------|--------|
| 分片能力 | ✗ 单机 | ✓ 原生分片 |
| 权限过滤 | SQL JOIN (慢) | Payload Filter (快) |
| 内存占用 | 40GB (1000万) | 20GB (量化压缩) |
| 写入阻塞 | ✓ 锁表 | ✗ 增量更新 |
| P95 延迟 | 280ms | 45ms |

**Qdrant 优化配置：**
- 量化压缩：Scalar Quantization INT8 (内存减半)
- HNSW 调优：m=16, ef_construct=200 (召回率 +5%)
- 6 分片 × 2 副本 = 12 份数据 (高可用)

**2. 全文检索：PostgreSQL FTS → Elasticsearch**

| 对比项 | PG FTS | Elasticsearch |
|--------|--------|---------------|
| 中文分词 | ✗ 弱 | ✓ ik_smart |
| 性能 | QPS 80 | QPS 1200 |
| 相关性 | ts_rank | BM25 可调优 |
| P95 延迟 | 450ms | 35ms |

**ES 索引设计：**
- ik_smart 中文分词器
- Mapping 优化：name (text + keyword), content (text)
- 预计算 full_path（避免查询时递归）

**3. Hybrid Search 并行化**

改造前（串行）：FTS 300ms + Vector 280ms = 580ms  
改造后（并行）：max(ES 35ms, Qdrant 45ms) = 45ms

**并发策略：**
- ES 全文检索与 Qdrant 向量检索并行执行
- RRF (Reciprocal Rank Fusion) Chunk 级融合
- Embedding 缓存（Redis）：热点查询 0 延迟

**4. Reranker 优化**
- 超时降级（500ms 未返回则跳过）
- 候选数阈值（<10 条跳过 Rerank）
- 批处理（10 个查询合并调用）

**5. 批处理优化**
- Embedding 批量 10 条（吞吐 8x）
- 两级缓存：LRU (本地) + Redis (分布式)

#### 性能对比

| 操作 | 改造前 P95 | 改造后 P95 | 提升 |
|------|-----------|-----------|------|
| Semantic Search | 280ms | 45ms | 6.2x |
| Fulltext Search | 450ms | 35ms | 12.9x |
| Hybrid Search | 580ms | 45ms | 12.9x |
| Ask (无 Rerank) | 2345ms | 2045ms | 1.15x |

#### 技术亮点
- Qdrant Payload Filter 下推（权限过滤在向量层）
- ES 异步构建（Kafka → ES Projector）
- 自适应 Reranker（超时降级不阻塞）

---

### 3.7 监控与可观测性增强

#### 目标
全方位监控，瓶颈可见，SLO 驱动运维。

#### 核心能力

**1. Grafana SLO Dashboard**
```
面板 1: 系统可用性 SLO
  • 目标: 99.9%
  • 当前: 实时计算 (2xx / total × 100%)
  • 告警: <99.5% 触发

面板 2: P95 延迟 SLO
  • 目标: <100ms
  • 分服务统计: identity/drive/upload/ai
  • 告警: >200ms 触发

面板 3: 熔断器状态
  • 各服务熔断器实时状态
  • Open/Half-Open/Closed 可视化
  • 熔断次数趋势

面板 4: 服务拓扑图
  • 自动生成调用关系图
  • 节点颜色表示健康状态
  • 边粗细表示流量大小
```

**2. 分布式追踪增强**
- 每步 Span 埋点：Embed/FTS/Vector/RRF/Rerank/LLM
- 耗时分解面板：识别瓶颈在哪一步
- 跨服务 Trace：Gateway → Services → DB/Cache

**3. 业务指标监控**
```
• 上传 QPS / 下载 QPS
• AI 检索延迟分布 (P50/P90/P95/P99)
• 向量索引速度 (chunks/sec)
• Redis 缓存命中率
• Kafka 消费延迟 (Lag)
• Embedding API 调用次数 / 费用
```

#### 技术亮点
- Prometheus + Thanos (长期存储)
- Jaeger 分布式追踪
- Grafana 统一可视化

---

## 四、最终架构形态

### 4.1 整体架构图

```
┌─────────────────────────────────────────────────────┐
│                 用户层 (Browser)                     │
└────────────────────┬────────────────────────────────┘
                     │ HTTPS
                     ↓
┌─────────────────────────────────────────────────────┐
│              Nginx (静态资源 + 反向代理)             │
└────────────────────┬────────────────────────────────┘
                     │
                     ↓
╔═════════════════════════════════════════════════════╗
║         API Gateway (自研 Go，800行)                ║
║  • 鉴权 • 限流 • 熔断 • 灰度 • 路由 • Trace       ║
╚════════════┬════════════════════════════════════════╝
             │ gRPC (服务发现 + 负载均衡)
    ┌────────┼────────┬─────────┬─────────┐
    ↓        ↓        ↓         ↓         ↓
┌────────┐┌──────┐┌───────┐┌────────┐┌──────┐
│Identity││Drive ││Upload ││Sharing ││  AI  │
│Service ││Svc   ││Service││Service ││Svc   │
└───┬────┘└──┬───┘└───┬───┘└───┬────┘└──┬───┘
    │        │        │        │        │
    └────────┴────────┴────────┴────────┘
             │
             ↓
    ┌────────────────┐
    │ Consul (配置)  │
    └────────────────┘
             │
             ↓
╔═════════════════════════════════════════════════════╗
║              事件驱动层 (Event-Driven)              ║
║  ┌────────────────────────────────────────────┐    ║
║  │ Kafka (事件总线)                            │    ║
║  │ Topics: file.events, folder.events, ...    │    ║
║  └────────┬───────────────────────────────────┘    ║
║           ↓                                         ║
║  ┌────────────────┐      ┌──────────────────┐     ║
║  │ Worker         │      │ ES Projector     │     ║
║  │ • 缩略图       │      │ Kafka → ES       │     ║
║  │ • AI 索引      │      │ (异步构建索引)   │     ║
║  │ • 对象校验     │      └──────────────────┘     ║
║  └────────────────┘                                ║
╚═════════════════════════════════════════════════════╝
             │
             ↓
╔═════════════════════════════════════════════════════╗
║                  数据持久化层                        ║
║  ┌──────────────────────────────────────────────┐  ║
║  │ PostgreSQL HA (Patroni + HAProxy)            │  ║
║  │ • Primary + 2 Standby                        │  ║
║  │ • 读写分离                                   │  ║
║  │ • 自动故障转移 (RTO 30s)                    │  ║
║  └──────────────────────────────────────────────┘  ║
║  ┌──────────────────────────────────────────────┐  ║
║  │ Redis Cluster (3主3从)                       │  ║
║  │ • Hash Slot 分片                             │  ║
║  │ • 缓存 + 队列                                │  ║
║  └──────────────────────────────────────────────┘  ║
║  ┌──────────────────────────────────────────────┐  ║
║  │ Qdrant Cluster (3节点)                       │  ║
║  │ • 6 分片 × 2 副本                            │  ║
║  │ • 量化压缩                                   │  ║
║  └──────────────────────────────────────────────┘  ║
║  ┌──────────────────────────────────────────────┐  ║
║  │ Elasticsearch (单节点)                       │  ║
║  │ • ik 中文分词                                │  ║
║  │ • 全文检索                                   │  ║
║  └──────────────────────────────────────────────┘  ║
║  MinIO / etcd (保持原样)                            ║
╚═════════════════════════════════════════════════════╝
             │
             ↓
╔═════════════════════════════════════════════════════╗
║             监控与可观测性层                         ║
║  Prometheus + Grafana + Jaeger + OTel Collector    ║
║  • SLO Dashboard                                    ║
║  • 服务拓扑图                                       ║
║  • 分布式追踪                                       ║
╚═════════════════════════════════════════════════════╝
```

### 4.2 容器清单对比

| 阶段 | 容器数 | 核心组件 |
|------|--------|---------|
| **改造前** | 9 | postgres, redis, minio, etcd, api, aisvc, worker, web, migrate |
| **改造后** | ~26 | +gateway, consul, haproxy, pg-standby×2, redis-cluster×6, qdrant×3, elasticsearch, zookeeper, kafka, es-projector, identity/drive/upload/sharing-svc |

### 4.3 技术栈对比

| 维度 | 改造前 | 改造后 |
|------|--------|--------|
| **应用架构** | 模块化单体 | 微服务 (5 服务) |
| **API 网关** | 无 | 自研 Go Gateway |
| **配置中心** | 环境变量 | Consul KV |
| **数据库** | PostgreSQL 单点 | Patroni HA + 读写分离 |
| **缓存** | Redis 单点 | Redis Cluster 3主3从 |
| **向量库** | pgvector 单机 | Qdrant Cluster 3节点 |
| **搜索** | PG FTS | Elasticsearch |
| **事件总线** | Outbox + Redis | Kafka + Outbox |
| **监控** | Prometheus + Jaeger | +Grafana SLO Dashboard |

---

## 五、关键技术决策

### 5.1 为什么不上 Kubernetes？

**当前选择：Docker Compose**

理由：
1. **开发验证阶段**：本地单机可完整运行，便于调试
2. **学习成本**：K8s 学习曲线陡，Docker Compose 简单直接
3. **资源约束**：K8s 需要至少 3 台机器，成本高
4. **可演进**：Helm Charts 已准备（作为"未来规划"）

**演进路径：**
- 当前：Docker Compose (开发/测试)
- 未来：K8s + Helm (生产环境)
- Compose → K8s 迁移成本低（容器不变，只改编排）

### 5.2 为什么自研 Gateway 而不用 Kong？

**选择：自研 Go Gateway (800行)**

理由：
1. **学习目的**：深入理解 Gateway 原理（限流/熔断/灰度）
2. **可控性**：完全掌控代码，便于定制和调试
3. **轻量级**：800 行代码，无额外依赖
4. **简历亮点**：展示架构设计能力

**生产环境：** 可替换为 Kong/Envoy（代码设计考虑了替换性）

### 5.3 为什么逻辑分库而不是物理分库？

**当前选择：Schema 隔离（逻辑分库）**

理由：
1. **事务简单**：跨 Schema 查询仍可用本地事务
2. **运维成本低**：单一 PostgreSQL 实例，备份恢复简单
3. **渐进演进**：为未来物理分库预留路径

**物理分库条件：**
- 单库 QPS > 10K (当前 5K)
- 数据量 > 1TB (当前 <500GB)
- 需要独立扩容数据库

### 5.4 为什么 Kafka 单节点？

**当前选择：Kafka 单节点（开发模式）**

理由：
1. **学习验证**：单节点足够验证事件驱动模式
2. **资源约束**：Kafka 集群需要 3 节点 + ZooKeeper
3. **数据可靠性**：Outbox 模式保证消息不丢（即使 Kafka 挂了也能重放）

**生产环境：** 升级为 3 节点集群（Replication Factor: 3）

---

## 六、性能指标预期

### 6.1 系统层面

| 指标 | 改造前 | 改造后 | 提升 |
|------|--------|--------|------|
| **可用性** | 99.0% | 99.9% | 10x (宕机时间) |
| **QPS** | 5,000 | 20,000 | 4x |
| **P95 延迟** | 300ms | 80ms | 3.75x |
| **RTO (恢复时间)** | >10min | <30s | 20x |
| **并发上传** | 50 | 200 | 4x |

### 6.2 RAG 性能

| 操作 | 改造前 P95 | 改造后 P95 | 提升 |
|------|-----------|-----------|------|
| **Semantic Search** | 280ms | 45ms | 6.2x |
| **Fulltext Search** | 450ms | 35ms | 12.9x |
| **Hybrid Search** | 580ms | 45ms | 12.9x |
| **向量索引** | 20 chunks/s | 160 chunks/s | 8x |

### 6.3 数据库性能

| 指标 | 改造前 | 改造后 | 提升 |
|------|--------|--------|------|
| **读 QPS** | 3,000 | 10,000 | 3.3x (读写分离) |
| **写 QPS** | 2,000 | 3,000 | 1.5x (主库专注写) |
| **主库故障恢复** | 手动 >10min | 自动 <30s | 20x |
| **查询延迟** | 80ms | 35ms | 2.3x (从库分摊) |

### 6.4 资源消耗

| 资源 | 改造前 | 改造后 | 变化 |
|------|--------|--------|------|
| **容器数** | 9 | 26 | +17 |
| **内存** | 16GB | 32GB | +16GB |
| **CPU** | 8核 | 16核 | +8核 |
| **存储** | 200GB | 500GB | +300GB |
| **向量内存** | 40GB (1000万) | 20GB (量化) | -50% |

---

## 七、风险与应对

### 7.1 技术风险

| 风险 | 影响 | 应对策略 |
|------|------|---------|
| **分布式事务复杂** | 跨服务操作难保证一致性 | Saga 模式补偿事务，保持逻辑分库降低复杂度 |
| **Kafka 消息丢失** | 事件投递失败 | Outbox 模式兜底，可重放；生产环境用副本 |
| **服务拆分过度** | 增加网络延迟 | gRPC 高性能通信，关键路径保持同步调用 |
| **Elasticsearch 单点** | 搜索服务故障 | 降级到 PG FTS；未来扩展为 3 节点 |
| **监控数据爆炸** | Prometheus 存储压力 | Thanos 长期存储，采样降低精度 |

### 7.2 运维风险

| 风险 | 影响 | 应对策略 |
|------|------|---------|
| **容器数增多** | 部署复杂度上升 | docker-compose 统一管理，Makefile 自动化脚本 |
| **配置复杂** | 易出错 | Consul 集中管理，配置版本化 |
| **日志分散** | 排查困难 | 集中日志（Loki），Trace 关联 |
| **数据迁移** | 停机时间 | Schema 迁移不停机，双写验证 |

### 7.3 时间风险

| 风险 | 影响 | 应对策略 |
|------|------|---------|
| **学习曲线** | 超出预期时间 | 每周交付可验证成果，可随时停止 |
| **调试困难** | 分布式问题难定位 | 完善的 Trace 和日志，每阶段充分测试 |
| **性能未达预期** | 需要返工 | 每阶段压测验证，及时调整 |

---

## 八、成功标准

### 8.1 阶段验收标准

**阶段 1：入口层增强**
- [ ] Gateway 能转发到现有 API
- [ ] 限流生效（超过阈值返回 429）
- [ ] 熔断器触发（5 次失败后快速失败）
- [ ] 灰度路由（Header 强制路由验证）

**阶段 2：数据层高可用**
- [ ] PG 主库宕机，30s 内自动恢复
- [ ] Redis Cluster 节点宕机，自动故障转移
- [ ] ES 全文检索性能 >10x PG FTS
- [ ] 读写分离延迟 <100ms

**阶段 3：事件驱动 + 服务拆分**
- [ ] 上传响应时间 <200ms（vs 改造前 35s）
- [ ] Kafka 消息投递成功率 >99.9%
- [ ] 5 个服务独立运行，服务间 gRPC 调用正常
- [ ] ES 索引自动构建（延迟 <1s）

**阶段 4：向量优化 + 监控**
- [ ] Qdrant 检索延迟 <50ms P95
- [ ] 向量内存占用减半（量化压缩）
- [ ] Grafana SLO Dashboard 显示实时可用性
- [ ] 压测报告：QPS 4x，延迟 3.75x

### 8.2 最终验收标准

**功能完整性：**
- [ ] 所有现有功能正常（注册/上传/搜索/问答/分享）
- [ ] 新增灰度发布能力
- [ ] 新增熔断降级能力

**性能指标：**
- [ ] 系统 QPS ≥ 20K
- [ ] P95 延迟 ≤ 100ms
- [ ] 可用性 ≥ 99.9%

**可运维性：**
- [ ] 单服务可独立扩容
- [ ] 配置可热更新（无需重启）
- [ ] 故障自动恢复（RTO <1min）
- [ ] Grafana 监控大盘完整

**文档完整性：**
- [ ] 架构图（C4 模型）
- [ ] 部署文档（一键启动）
- [ ] 压测报告（性能数据）
- [ ] ADR 决策记录

---

## 九、与其他方案对比

### 9.1 方案对比

| 方案 | 复杂度 | 时间 | 简历效果 | 落地性 | 推荐度 |
|------|--------|------|---------|--------|--------|
| **保持单体** | ⭐ | 0 周 | ⭐⭐ | ⭐⭐⭐⭐⭐ | ⭐ |
| **基础微服务** | ⭐⭐⭐ | 4 周 | ⭐⭐⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐⭐⭐ |
| **增强版微服务** (本方案) | ⭐⭐⭐⭐ | 6 周 | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐⭐⭐⭐ |
| **完整云原生** (K8s+Mesh) | ⭐⭐⭐⭐⭐ | 3 个月 | ⭐⭐⭐⭐⭐ | ⭐⭐ | ⭐⭐ |

### 9.2 为什么选择增强版微服务？

**vs 保持单体：**
- 单体无法展示架构设计能力
- 无法应对扩展性问题
- 简历缺乏亮点

**vs 基础微服务：**
- 增强版增加了流量治理（熔断/灰度）
- 增加了事件驱动（Kafka + CQRS）
- 增加了数据库高可用（Patroni）
- 技术深度更足，面试更有说服力

**vs 完整云原生：**
- K8s + Service Mesh 学习成本过高（3-6 个月）
- 资源消耗大（至少 3 台机器）
- 简历可能显得"不真实"（一个人真能做这么多？）
- 增强版已足够展示架构能力

---

## 十、总结

### 10.1 核心升级点

1. **高可用**：消除所有单点故障，99.9% 可用性
2. **微服务**：5 个服务独立部署扩容，故障隔离
3. **流量治理**：自研 Gateway，限流/熔断/灰度
4. **事件驱动**：Kafka + CQRS，异步解耦
5. **数据库 HA**：Patroni 读写分离，自动故障转移
6. **RAG 优化**：Qdrant + ES，性能 10x

### 10.2 技术亮点

- 自研 API Gateway（800 行 Go）
- 自适应熔断器（滑动窗口算法）
- Patroni 自动故障转移（RTO 30s）
- Redis Cluster 分片
- Qdrant 量化压缩（内存减半）
- Kafka 事件驱动 + CQRS
- Grafana SLO 监控

### 10.3 面试价值

**架构设计能力：**
- 从单体到微服务的演进路径
- 服务拆分边界（DDD）
- 流量治理（限流/熔断/灰度）
- 高可用设计（故障转移/读写分离）

**技术广度：**
- Go、gRPC、Protobuf
- PostgreSQL、Redis、Kafka、Elasticsearch、Qdrant
- Patroni、Consul、etcd
- Prometheus、Grafana、Jaeger

**技术深度：**
- 自研 Gateway 深入理解原理
- 自适应熔断器算法
- Transactional Outbox 模式
- CQRS 读写分离
- 分布式追踪

**可落地性：**
- 6 周可完成
- Docker Compose 本地可运行
- 每个技术点都有真实代码
- 性能数据可量化

---

## 附录

### A. 技术栈清单

**编程语言：** Go 1.25, JavaScript (React)

**应用框架：** Gin, gRPC, React 18

**数据库：** PostgreSQL 15 + pgvector, Patroni, HAProxy

**缓存：** Redis 7 (Cluster 模式)

**消息队列：** Kafka 3.x, Asynq

**向量数据库：** Qdrant 1.13

**搜索引擎：** Elasticsearch 8.11 + ik 分词

**对象存储：** MinIO (S3 兼容)

**服务发现：** etcd v3, Consul

**可观测性：** Prometheus, Grafana, Jaeger, OpenTelemetry

**容器编排：** Docker Compose

**AI 服务商：** DashScope (Alibaba Cloud)

### B. 关键指标定义

**可用性 (Availability)：**
```
Availability = (Total Time - Downtime) / Total Time × 100%
99.9% = 每月宕机 43.2 分钟
99.0% = 每月宕机 7.2 小时
```

**RTO (Recovery Time Objective)：**
```
故障发生到服务恢复的时间
目标: <30s (Patroni 自动故障转移)
```

**RPO (Recovery Point Objective)：**
```
数据丢失容忍度
目标: 0 (同步复制)
```

**P95 延迟：**
```
95% 的请求延迟低于此值
目标: <100ms
```

### C. 参考资料

**开源项目：**
- ownCloud Infinite Scale (oCIS): Go 微服务云盘
- JuiceFS: 分布式文件系统（元数据服务化）
- Seafile: 企业级云盘（集群部署）

**架构模式：**
- Microservices Patterns (Chris Richardson)
- Building Microservices (Sam Newman)
- Designing Data-Intensive Applications (Martin Kleppmann)

**技术博客：**
- Uber Engineering: Microservices Architecture
- Netflix Tech Blog: Hystrix Circuit Breaker
- Alibaba Cloud: DashScope RAG Best Practices

---

**文档结束**

下一步：进入实施阶段，参考 `docs/bx_yunpan_升级执行计划书.md` 的具体任务分解。
