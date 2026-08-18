# BX YunPan ERD

- 版本：2.0
- 日期：2026-08-14
- 状态：M0 逻辑模型

本文是逻辑 ERD。字段类型、部分唯一索引、状态约束和迁移顺序以版本化 SQL Migration 为最终事实来源。

```mermaid
erDiagram
    users ||--o{ refresh_sessions : owns
    users ||--|| folders : has_root
    users ||--o{ folders : owns
    folders ||--o{ folders : contains
    users ||--o{ file_entries : owns
    folders ||--o{ file_entries : contains
    file_objects ||--o{ file_entries : referenced_by

    users ||--o{ upload_sessions : starts
    folders ||--o{ upload_sessions : receives
    upload_sessions ||--o{ upload_parts : records
    upload_sessions o|--o| file_objects : completes_as
    upload_sessions o|--o| file_entries : creates

    users ||--o{ shares : creates
    file_entries ||--o{ shares : shared_as
    shares ||--o{ share_imports : imported_as
    users ||--o{ share_imports : imports
    folders ||--o{ share_imports : destination

    file_objects ||--o{ object_variants : derives
    file_objects ||--o{ ai_documents : indexed_as
    ai_documents ||--o{ ai_chunks : contains

    users o|--o{ async_tasks : observes
    users o|--o{ audit_logs : acts

    users {
        uuid id PK
        varchar username_normalized UK
        varchar email_normalized UK
        text password_hash
        smallint status
        bigint quota_bytes
        bigint used_logical_bytes
        bigint reserved_bytes
        timestamptz created_at
    }

    refresh_sessions {
        uuid id PK
        uuid user_id FK
        bytea token_hash UK
        uuid family_id
        timestamptz expires_at
        timestamptz revoked_at
    }

    folders {
        uuid id PK
        uuid owner_id FK
        uuid parent_id FK
        varchar name
        varchar name_normalized
        timestamptz deleted_at
        bigint version
    }

    file_entries {
        uuid id PK
        uuid owner_id FK
        uuid folder_id FK
        uuid object_id FK
        varchar name
        varchar name_normalized
        timestamptz deleted_at
        bigint version
    }

    file_objects {
        uuid id PK
        char sha256
        bigint size_bytes
        varchar mime_type
        varchar bucket
        varchar object_key UK
        smallint status
        bigint reference_count
        timestamptz verified_at
        timestamptz deleted_at
    }

    upload_sessions {
        uuid id PK
        uuid user_id FK
        uuid folder_id FK
        varchar filename
        char declared_sha256
        bigint size_bytes
        varchar object_key
        varchar storage_upload_id
        bigint reserved_bytes
        smallint status
        varchar idempotency_key
        uuid completed_object_id FK
        uuid completed_entry_id FK
        bigint version
    }

    upload_parts {
        uuid session_id PK,FK
        int part_number PK
        varchar etag
        bigint size_bytes
        varchar checksum
    }

    shares {
        uuid id PK
        uuid owner_id FK
        uuid file_entry_id FK
        bytea key_hash UK
        timestamptz expires_at
        timestamptz revoked_at
        bigint version
    }

    share_imports {
        uuid id PK
        uuid share_id FK
        uuid user_id FK
        uuid target_folder_id FK
        varchar idempotency_key
        uuid imported_entry_id FK
    }

    object_variants {
        uuid id PK
        uuid object_id FK
        varchar variant_type
        varchar object_key UK
        varchar mime_type
        int width
        int height
        varchar pipeline_version
        smallint status
    }

    ai_documents {
        uuid id PK
        uuid object_id FK
        varchar extractor_version
        text summary
        jsonb tags
        varchar language
        smallint status
        varchar model_version
    }

    ai_chunks {
        uuid id PK
        uuid document_id FK
        int chunk_index
        text content
        tsvector content_tsv
        int page_number
        varchar section
        vector embedding
        varchar embedding_model
    }

    async_tasks {
        uuid id PK
        uuid owner_id FK
        varchar task_type
        varchar dedupe_key
        varchar resource_type
        uuid resource_id
        smallint status
        int progress
        int attempt
    }

    outbox_events {
        uuid id PK
        varchar aggregate_type
        uuid aggregate_id
        varchar event_type
        int event_version
        jsonb payload
        smallint status
        timestamptz available_at
        int attempt
    }

    event_consumptions {
        uuid event_id PK
        varchar consumer_name PK
        timestamptz processed_at
    }

    audit_logs {
        uuid id PK
        uuid actor_id FK
        varchar action
        varchar resource_type
        uuid resource_id
        varchar request_id
        inet ip
        jsonb metadata
    }
```

## 1. 关键唯一约束

| 表 | 约束 |
|---|---|
| `folders` | 每用户一个 active 根目录；active 子目录 `(owner_id, parent_id, name_normalized)` 唯一 |
| `file_entries` | active `(owner_id, folder_id, name_normalized)` 唯一 |
| `file_objects` | `(sha256, size_bytes)` 唯一 |
| `upload_sessions` | `(user_id, idempotency_key)` 唯一 |
| `share_imports` | `(user_id, share_id, idempotency_key)` 唯一 |
| `object_variants` | `(object_id, variant_type, pipeline_version)` 唯一 |
| `ai_documents` | `(object_id, extractor_version, model_version)` 唯一 |
| `ai_chunks` | `(document_id, chunk_index, embedding_model)` 唯一 |
| `async_tasks` | `(task_type, dedupe_key)` 唯一 |
| `event_consumptions` | `(event_id, consumer_name)` 主键 |

## 2. 分享与审计引用

`shares.file_entry_id` 只引用单个 FileEntry。Sharing 模块在创建、解析和导入时校验文件属于 Share Owner、Entry 未删除、Share 未撤销且未过期。导入事务创建新的 FileEntry 并增加同一 FileObject 的引用，不复制对象内容。

`audit_logs.resource_type + resource_id` 是审计引用，不参与业务一致性判断。

## 3. 引用与配额不变量

```text
file_objects.reference_count >= 0
users.used_logical_bytes >= 0
users.reserved_bytes >= 0
users.used_logical_bytes + users.reserved_bytes <= users.quota_bytes
completed upload_session => completed_object_id and completed_entry_id are not null
active file_entry => referenced file_object is ready
```

`reference_count` 是事务内维护的快速计数。GC 删除对象前必须再次执行真实 FileEntry 引用查询，不能只相信缓存计数。

## 4. 删除语义

- Folder/FileEntry 删除后立即对用户不可见，不提供恢复入口。
- 删除 FileEntry 的事务立即减少 Object 引用和逻辑用量，并写入 Outbox。
- Object 引用归零后只进入 GC 候选，宽限期后由 Worker 二次确认并删除 MinIO 对象。
- Share 源 Entry 删除后立即不可访问；已经导入的 Entry 不受影响。
- Folder 只允许为空时删除；内部 `deleted_at` 只服务于幂等和 GC，不代表用户回收站。
