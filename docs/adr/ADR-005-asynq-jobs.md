# ADR-005：使用 Asynq 承载后台任务

- 状态：Accepted
- 日期：2026-08-14
- 决策者：项目维护者

## 背景

目标系统存在对象强校验、缩略图、OCR、文档解析、Embedding、GC、上传过期和分享过期等后台任务。它们具有不同并发、超时、重试和资源特征，需要独立于 HTTP 请求执行。

项目已经使用 Redis 处理限流和短期状态。当前规模不需要 Kafka 的长期事件流和分区运维，但简单 goroutine 或数据库状态轮询缺少可靠重试、调度和队列隔离。

## 决策

使用 Asynq 作为首版后台任务队列，Redis 作为 Broker。任务按以下队列隔离：

- `media`：探测、缩略图、预览变体。
- `ai`：提取、Vision、Chunk、摘要、Embedding。
- `object`：对象强校验和 GC。
- `maintenance`：上传、分享过期和补偿任务。

Worker 使用一个 Go 二进制，通过队列权重和角色配置独立部署。每类 Task 必须声明稳定类型、幂等键、最大重试、Timeout、Retention、用户可见性及可重试/永久错误分类。

领域事件仍由 PostgreSQL Outbox 保存；Asynq 是执行队列，不是业务事实源。

## 后果

正面影响：

- 与 Go、Redis 和本地 Compose 集成简单。
- 提供重试、延迟、超时、队列权重和任务保留。
- Media、AI、System 可以独立扩容。
- 演示环境不需要维护 Kafka 集群。

代价：

- Redis 数据丢失时队列任务可能需要由 Outbox/补偿重新发布。
- 不适合作为长期不可变事件日志。
- 任务成功但 Ack 前退出会重复处理，消费者必须幂等。

## 被否决方案

### 请求内 goroutine

否决原因：进程退出丢任务，缺少重试、状态、背压和资源隔离。

### Kafka

否决原因：当前没有事件吞吐、长期回放或多消费组规模来支撑额外运维成本；未来跨服务事件流可以演进到 Kafka 或 NATS JetStream。

### 纯数据库任务队列

否决原因：可行但需要自行实现调度、重试、超时和队列权重；PostgreSQL 已承担业务事实与 Outbox，不再增加通用执行队列职责。

## 约束

- Task Payload 只传 ID 和版本，不传大文本、文件内容或 Secret。
- 所有 Handler 接收 `context.Context` 并设置超时。
- 重试必须有上限和退避；永久错误不能无限重试。
- Task 状态和用户可见进度写入 PostgreSQL，不能只存在 Redis。
