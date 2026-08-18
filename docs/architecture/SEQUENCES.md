# BX YunPan 核心时序

- 版本：2.0
- 日期：2026-08-14
- 状态：M0 基线

## 1. Multipart 上传与强校验

```mermaid
sequenceDiagram
    autonumber
    actor User
    participant Web as React SPA
    participant API as Go API
    participant DB as PostgreSQL
    participant S3 as MinIO
    participant Sys as System Worker
    participant Q as Asynq/Redis

    User->>Web: 选择文件与目标目录
    Web->>Web: Web Worker 增量计算 SHA-256
    Web->>API: POST /uploads + Idempotency-Key
    API->>DB: 检查目录、配额、同用户已拥有对象
    alt 同用户秒传命中 ready Object
        API->>DB: 事务创建 FileEntry、增加引用、写 Outbox
        API-->>Web: 201 instant + FileEntry
    else 需要上传
        API->>S3: CreateMultipartUpload(temp key)
        API->>DB: 创建 UploadSession 并预占配额
        API-->>Web: 201 multipart + session
        loop 每组 Part
            Web->>API: POST /parts/presign
            API-->>Web: 短期 UploadPart URL
            Web->>S3: PUT Part
            S3-->>Web: ETag / Checksum
            Web->>API: POST /parts/confirm
            API->>DB: 幂等记录 Part
        end
        Web->>API: POST /complete
        API->>S3: CompleteMultipartUpload
        API->>DB: Session -> verifying，写 object.verify Outbox
        API-->>Web: 202 verifying
        Sys->>DB: 领取 Outbox
        Sys->>Q: 发布 object:verify
        Sys->>S3: 流式读取临时对象并计算 SHA-256
        alt Hash 与大小正确
            Sys->>DB: 事务创建/复用 FileObject、创建 FileEntry、释放预占、Session -> completed
            opt 已存在相同 ready Object
                Sys->>S3: 异步删除重复临时对象
            end
            Sys-->>Web: SSE task.completed
        else Hash 或大小错误
            Sys->>DB: Session -> failed、释放预占、记录错误
            Sys->>S3: 删除临时对象
            Sys-->>Web: SSE task.failed
        end
    end
```

关键不变量：

- `completed` Session 必须同时有 ObjectID 和 EntryID。
- ready FileEntry 只能引用 ready FileObject。
- 客户端 Hash 用于候选匹配，服务端流式 Hash 才能建立可信 Object。
- 重试 `POST /uploads`、`POST /complete` 和 Worker 任务不得创建重复 Entry 或增加两次引用。
- 数据库事务中不调用 MinIO；失败后的外部副作用通过补偿任务完成。

## 2. Share Key 解析与导入

```mermaid
sequenceDiagram
    autonumber
    actor Owner as 分享者
    actor Guest as 访问者
    participant API as Go API
    participant DB as PostgreSQL

    Owner->>API: POST /shares
    API->>DB: 校验单个 FileEntry 的归属和 active 状态
    API->>API: 生成 128-bit 随机 Key
    API->>DB: 保存 HMAC(Key)、过期时间和 Outbox
    API-->>Owner: 201 + 明文 Share Key（仅一次）

    Guest->>API: POST /public/shares/resolve + Key
    API->>DB: 等值查 HMAC(Key)
    API->>DB: 校验撤销、过期和源 Entry 状态
    API-->>Guest: 短期 Share Access Token + 元数据
    Guest->>API: GET /public/shares/content + X-Share-Token
    API-->>Guest: 授权预览元数据

    Guest->>API: POST /shares/{id}/import + X-Share-Token + Idempotency-Key
    API->>DB: 校验登录、Token 和目标目录
    API->>DB: 锁定 Share、源 Entry 和目标目录
    API->>DB: 事务创建 FileEntry、增加对象引用、写 ShareImport/Outbox/Audit
    API-->>Guest: 201 imported FileEntry

    Guest->>API: 使用相同 Idempotency-Key 重试
    API->>DB: 查询既有 ShareImport
    API-->>Guest: 返回相同 imported FileEntry
```

固定语义：

- Share 是实时引用，不保存内容快照。
- 源 Entry 被删除或失去 active 状态后，访问与后续导入失效。
- 已经导入的 FileEntry 是独立逻辑引用，不受 Share 后续撤销或源删除影响。
- 导入不复制物理对象；文件重名返回 409，由用户选择新名称后重试。

## 3. AI 索引与权限感知问答

```mermaid
sequenceDiagram
    autonumber
    participant Sys as System Worker
    participant Q as Asynq/Redis
    participant AIW as AI Worker
    participant S3 as MinIO
    participant Model as AI Provider
    participant DB as PostgreSQL/pgvector
    actor User
    participant API as Go API

    Sys->>Q: 发布 ai:index_requested(object, versions)
    Q->>AIW: 投递幂等任务
    AIW->>DB: 检查 object + pipeline + model 唯一键
    alt 已索引
        AIW-->>Q: success
    else 未索引
        AIW->>S3: 流式读取授权对象
        AIW->>AIW: Extract / OCR / Chunk
        AIW->>Model: Summary / Embedding / Vision
        Model-->>AIW: 模型结果与用量
        AIW->>DB: 事务写 AIDocument、AIChunk、向量与状态
        AIW-->>Q: ack
    end

    User->>API: POST /search 或 POST /ai/ask
    API->>DB: 在用户 active Entry 范围内执行 FTS
    API->>DB: 在相同授权范围内执行 vector search
    API->>API: RRF 融合、过滤、构造 Citation
    opt 问答
        API->>Model: 仅发送授权 Chunk 和 Citation ID
        Model-->>API: 带 Citation ID 的回答
        API->>API: 校验引用可解析且仍有权限
    end
    API-->>User: 文件、片段、分数、页码/段落引用
```

故障规则：

- 429、5xx、网络超时可重试；不支持的文件与无效模型响应进入明确失败状态。
- AI 失败不影响原文件下载和目录操作。
- 相同 FileObject 在相同 Pipeline/模型版本下只索引一次。
- 权限过滤必须进入 SQL，不允许全局向量召回后只靠应用层丢弃越权结果。
- 回答引用必须映射到当前用户可访问的 FileEntry 和 AIChunk。
