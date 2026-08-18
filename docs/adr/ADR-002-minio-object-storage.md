# ADR-002：使用 MinIO/S3 替代 FastDFS

- 状态：Accepted
- 日期：2026-08-14
- 决策者：项目维护者

## 背景

旧系统同时通过 FastDFS C API、命令行工具和 Nginx 模块读写文件，并把 FastDFS FileID 与公开 URL 写入业务数据。Go 生态下可以继续封装 FastDFS 客户端，但这会保留专有协议、部署复杂度和迁移负担，也不能自然支持浏览器 Multipart 预签名直传。

项目需要：

- 大文件数据绕过 Go API。
- Multipart、断点续传和短期下载授权。
- 本地一条命令部署。
- 将来切换公有云 S3/OSS 时不重写业务模型。

## 决策

本地和演示环境使用 MinIO，应用只依赖内部 `ObjectStorage` Port 和 S3 语义。

对象存储规则：

- 原文件和变体都在私有 Bucket。
- Object Key 完全由服务端生成，不包含用户提供的路径。
- 浏览器通过短期预签名 URL 直接上传 Part 和下载对象。
- 内部对象存储端点和浏览器公开签名端点独立配置；本地通过同源 `/storage` 反向代理访问，代理保留 Host 并剥离该前缀。
- UploadSession 保存临时 Key；服务端强校验后才创建或复用正式 FileObject。
- 数据库保存 Bucket、Object Key、Hash、大小和状态，不保存永久公开 URL。
- 删除采用引用检查、宽限期和异步 GC。

旧 FastDFS 不进入新系统运行时。首版从空 PostgreSQL/MinIO 启动，不建设自动化旧数据迁移工具；确有数据保留需求时再作为独立项目评估。

## 后果

正面影响：

- 使用成熟、通用的 S3 API 和 Go SDK。
- API 不代理大文件，内存与带宽压力可控。
- Multipart、预签名和生命周期能力完整。
- 可迁移到 AWS S3、阿里云 OSS 的 S3 兼容接口或专用 Adapter。

代价：

- 数据库与对象存储不能使用同一个事务，需要状态机、补偿和孤儿清理。
- Multipart ETag 不是文件 SHA-256，必须进行服务端强校验。
- 本地 Compose 增加 MinIO 和初始化 Job。

## 被否决方案

### 保留 FastDFS

否决原因：Go 集成与运维收益不足，无法体现现代对象存储的直传和云迁移能力。

### 文件全部经过 Go API

否决原因：API 内存、连接和出口带宽随文件增长，无法满足控制面与数据面分离目标。

### 直接依赖单一公有云 OSS SDK

否决原因：本地演示需要外部账号和费用，并把业务代码绑定到供应商。

## 约束

- 不用 Multipart ETag 代替完整文件 Hash。
- 预签名 URL 必须绑定方法、Key、Part Number 和短有效期。
- MinIO 调用不得放在数据库事务内部。
- 物理删除前必须查询真实逻辑引用。
