# ADR-003：使用 PostgreSQL 与 pgvector

- 状态：Accepted
- 日期：2026-08-14
- 决策者：项目维护者

## 背景

旧系统使用 MySQL 保存元数据，并为每个用户维护本地 FAISS 文件。该设计存在索引重建、并发访问、权限过滤、备份和多副本部署困难。目标系统同时需要关系事务、递归目录、全文检索、Outbox、JSON 元数据和向量检索。

展示版本的数据规模不需要独立 Elasticsearch 和向量数据库，但需要把权限条件放进检索查询，避免全局召回后在应用层过滤导致越权风险。

## 决策

PostgreSQL 作为唯一业务事实数据库，并启用：

- `vector`：AI Chunk Embedding 与相似度查询。
- `pg_trgm`：文件名模糊检索。
- PostgreSQL FTS：Chunk 全文检索。

常规持久化使用 GORM，Migration、递归 CTE、`FOR UPDATE SKIP LOCKED`、FTS 和 pgvector 热点查询使用显式 SQL。数据库结构只由 golang-migrate 管理，不使用 GORM AutoMigrate。

混合搜索在同一数据库中执行授权 FileEntry 过滤、FTS 召回和向量召回，并使用 RRF 融合。向量索引在数据量证明有必要后选择 HNSW；小规模评测优先精确扫描，保留 Recall 基线。

## 后果

正面影响：

- 关系事务、权限过滤、全文和向量检索位于同一数据源。
- 减少 Elasticsearch、Milvus、FAISS 文件服务等额外基础设施。
- Outbox、审计、任务状态和元数据可统一备份。
- 查询可以直接连接 FileEntry 权限范围，降低检索泄漏风险。

代价：

- PostgreSQL 同时承担元数据和搜索负载，需要索引与查询观测。
- pgvector 在超大规模向量下可能需要拆分专用检索服务。
- GORM 不能优雅表达所有复杂查询，需要接受显式 SQL。

## 被否决方案

### MySQL + FAISS 文件

否决原因：本地文件索引不适合多副本、事务更新和权限感知查询。

### PostgreSQL + Elasticsearch + Milvus

否决原因：首版基础设施重复，数据同步和故障面过大，无法用当前规模证明必要性。

### 只使用向量检索

否决原因：文件名、编号、专有名词等精确词法查询会明显退化，需要 FTS 与 Vector 混合召回。

## 约束

- 集成测试使用真实 PostgreSQL + pgvector，不能用 SQLite 替代。
- Search 权限条件必须进入 SQL。
- Embedding 模型和维度变化必须版本化。
- 性能结论必须记录数据量、索引、查询计划和 Recall 变化。
