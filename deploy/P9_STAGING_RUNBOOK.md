# P9 Staging Runbook

本 runbook 只适用于已命名的非生产 staging 环境。当前单机 Compose 不满足这些前置条件。

## 进入门禁

先由值班负责人以环境变量注入 endpoint（不要把密码、token 或 API key 写入文档），再执行：

```bash
./deploy/staging-ha-check.sh
./deploy/staging-search-check.sh
./deploy/staging-p9-check.sh
```

三个检查脚本必须都返回 `0`。返回 `2` 表示环境缺少必要参数，不是代码通过。

## 固定顺序

1. 备份 PostgreSQL、Redis AOF/RDB、对象存储 inventory 和 Kafka consumer offsets；记录 backup id、时间、操作者和目标 RPO/RTO。
2. 在不写入业务事实的条件下恢复到隔离 staging，执行 checksum、行数、对象 size/hash 和索引版本校验。
3. 演练 PostgreSQL planned failover：暂停新 writer、提升 replica、fence 旧 primary、确认唯一 writer，再恢复应用连接。事务结果不确定时使用原 Idempotency-Key 查询/重试。
4. 演练 Redis Broker 切换和全丢重建：暂停 Relay enqueue，切 endpoint，恢复 Relay，运行 PostgreSQL reconciliation，确认没有无法解释的任务。
5. 演练 MinIO inventory copy、checksum 校验、预签名上传/下载、分享访问和 GC；旧端保持只读直到 observation window 结束。
6. 创建新的 `index_epoch`，从 PostgreSQL snapshot 重建 ES/Qdrant，追平 watermark，执行 shadow query 和权限泄漏检查，最后通过 alias/flag 原子切换。
7. 启用 Kafka delivery，验证 producer ack、重复/乱序、consumer dedupe、DLQ 和指定 watermark replay；Kafka 不可用时核心事务必须仍可提交。
8. 最后执行 Gateway TLS/续期、请求 ID/trace、限流、后端摘除和浏览器关键路径。物理分库必须在 P8 领域 canary、Saga、唯一 writer 和 data failback 演练之后单独发布。

## 回滚规则

- PostgreSQL：停止新 writer，确认目标停止写并完成 fencing/checksum 后才允许回指旧 primary。
- Redis：停止 Relay 新投递，保留 PostgreSQL Outbox 权威，按 reconciliation 重建，不合并未知队列。
- MinIO：写冻结；只有反向同步和 checksum 证明一致时才切回旧 endpoint。
- ES/Qdrant：将 `SEARCH_READ_BACKEND` 切回 `postgres` 或旧 alias，不反向写事实库。
- Kafka：停止 Kafka delivery/consumer，保留 topic/offset，搜索回到 Asynq/PG snapshot。
- Gateway：DNS/LB 权重切回已验证入口；Cookie Secure 和公共 S3 scheme 必须与浏览器实际协议一起回退。

每一步都必须保存命令输出、时间线、故障注入点、观测指标和负责人；没有证据就保持当前阶段，不宣布 P9 完成。
