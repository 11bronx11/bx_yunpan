# BX YunPan Go 测试方案

- 版本：2.0
- 日期：2026-08-14
- 原则：核心风险优先，保证开发速度

## 1. 目标

测试只证明核心主链路和高风险不变量，不追求生产级穷举，也不设置为了数字而写测试的覆盖率硬门槛。

优先保证：

- 用户不能访问他人文件、搜索结果或 Share。
- 上传完成、重试和并发不会产生重复对象或重复 Entry。
- 删除一个 Entry 不会误删仍被引用的物理对象。
- Worker 重试不会重复生成缩略图或 AI Chunk。
- AI 检索和问答只使用用户有权访问的文件。

## 2. 分层策略

### 2.1 单元测试

只覆盖有明确状态和边界的逻辑：

- Token、密码、Share Key 和权限策略。
- UploadSession 状态机与幂等键。
- 文件名、目录移动、配额和 Hash 规则。
- FileObject 引用与 GC 判断。
- Chunk、RRF 和 Citation 整理。
- Worker 错误分类与重试判断。

不为简单 Getter、GORM 映射和框架内部行为写测试。

### 2.2 集成测试

使用 testcontainers-go 启动 PostgreSQL/pgvector、Redis 和 MinIO。集中覆盖：

- Migration 从空库执行。
- 注册、登录和 Refresh 轮换。
- 目录唯一约束和跨用户权限。
- Multipart、Complete、强校验、秒传和删除 GC。
- Share Key 导入复用 FileObject。
- Outbox 领取、重复发布和幂等消费。
- AI 文档、Chunk、FTS/Vector 查询和权限过滤。

不使用 SQLite 替代 PostgreSQL。

### 2.3 前端测试

复用现有 React 测试体系，只增加关键行为：

- 登录状态和 Token 刷新。
- 目录切换与面包屑。
- 上传进度、暂停和恢复。
- Share Key 导入。
- AI 搜索结果和引用点击。

不测试 Ant Design 内部实现，不追求所有页面快照。

### 2.4 Playwright E2E

只保留五条演示主路径：

1. 注册、登录和退出。
2. 创建目录、上传文件、刷新后继续上传、下载。
3. 同文件秒传、删除 Entry 后另一引用仍可用。
4. 创建 Share Key，第二账号导入到目录。
5. 上传 PDF/图片，等待 AI 索引，搜索并完成带引用问答。

不要求每次 PR 运行完整浏览器矩阵；CI 默认 Chromium。

### 2.5 性能 Smoke

只保留三个 k6 场景：

- 目录列表与文件元数据。
- 创建 UploadSession 和幂等 Complete。
- 混合搜索。

记录环境、数据量、并发和 p95，不声明生产容量，不建立大规模容量门禁。

## 3. 核心用例

### Identity

- 用户名重复返回 409。
- 错误密码返回统一 401。
- Refresh Token 轮换后旧 Token 失效。
- 跨用户资源 ID 不返回敏感元数据。

### Drive/Object/Upload

- 同目录重名返回 409。
- 目录不能移动到自身子孙。
- 非空目录删除被拒绝。
- 上传中断后能查询已完成 Part。
- Complete 重试返回同一 Session 结果。
- Hash 不匹配不创建 ready FileObject/FileEntry。
- 同用户秒传只创建新 Entry。
- 跨用户仅能通过 Share 导入复用对象。
- 引用归零前 MinIO 对象不能删除。
- GC 重复执行幂等。

### Sharing

- 数据库只保存 Share Key HMAC。
- 过期、撤销和源文件删除后不可访问或导入。
- 同一 Idempotency-Key 重试不重复导入。
- 导入只创建 FileEntry，不复制物理对象。

### AI/Search

- 文本、PDF、DOCX 和图片可完成对应解析流程。
- 相同 Object/Pipeline/模型版本不重复索引。
- Provider 429/5xx/超时按策略重试。
- 用户 A 搜索和问答不返回用户 B 独占内容。
- Citation 能映射到当前可见文件和页码/段落。
- AI 失败不影响文件下载。

## 4. 小型 AI 评测

保留 20-30 个固定文件，包含文本、PDF、DOCX 和图片。只检查：

- 文件级 Recall@5。
- Chunk 级 Recall@10。
- 权限泄漏为 0。
- Citation 文件映射正确。
- Mock Provider 下 Pipeline 可重复通过。

Recall 只作为调参参考，不作为阻塞整个项目的硬门槛；权限泄漏和错误引用属于必须修复的问题。

## 5. CI 门禁

### Pull Request

```text
gofmt
go vet
go test ./...
frontend lint + targeted unit
frontend build
OpenAPI lint
migration up on empty database
```

### Main

增加：

```text
core testcontainers integration
one Chromium E2E smoke
container build
```

### 发布演示前

```text
five core E2E paths
go test -race on core packages
three k6 smoke scenarios
AI small evaluation set
manual three-minute walkthrough
```

## 6. 明确取消的严苛要求

- 不设置后端整体 70% 或领域 85% 的硬覆盖率。
- 不要求 P0/P1 所有场景全部自动化。
- 不要求 E2E 连续三次全量无偶发失败。
- 不在每个 PR 运行 Race Detector、完整 Testcontainers 和 Playwright。
- 不建设完整故障注入矩阵。
- 不做 100,000 Chunk/10,000 Task 等大规模压力门禁。
- 不做旧数据两次全量迁移演练和备份恢复 Drill。
- 不要求完整多浏览器兼容矩阵。

## 7. 施工包完成条件

每个业务切片只需满足：

- 主正常路径有测试。
- 权限、幂等或数据一致性风险至少有一条失败路径测试。
- OpenAPI 与实现一致。
- 前端具备 loading、empty 和 error 状态。
- 手工 Smoke 能完成该切片主流程。

测试报告只陈述实际执行结果，不把静态检查、本地单机数据或 Mock AI 描述为生产能力。
