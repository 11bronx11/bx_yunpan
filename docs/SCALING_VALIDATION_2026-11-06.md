# 独立服务扩缩容验证（2026-11-06）

## 环境与范围

在当前 `bx-fullchain-20261106` 完整拆分栈执行，基线为 28 个运行容器。ES/Qdrant 检索、PostgreSQL 降级、独立 worker、AI query/indexer、远程解析和真实 DashScope 保持开启。旧合并 worker/aisvc 为 0 副本。

本轮验证应用服务副本的独立增加和减少。PostgreSQL、Redis、RabbitMQ、MinIO、etcd、ES/Qdrant 为现有单机依赖，不将它们作为无状态进程直接增加副本。未验证跨主机数据集群扩缩容、自动扩缩容或容量提升倍数。Web 保持固定入口 `127.0.0.1:24000`，持续通过它验证路由与业务可用性。

每次只调整一类服务，完成后恢复基线再继续下一类；不删除数据卷，不改业务 `.env`，不重新构建或替换现有镜像。所有 Compose 操作均包含 `compose.sharing.yaml`，避免把分享版本回退。

## 方法

测试脚本：`deploy/fullchain-scale-test.mjs`。默认覆盖全部 10 类角色，也可传角色名称分别执行：

```bash
FULLCHAIN_SCALE_OUTPUT=artifacts/scaling-20261106/online-verified \
  node deploy/fullchain-scale-test.mjs api ai-query

FULLCHAIN_SCALE_OUTPUT=artifacts/scaling-20261106/workers \
  node deploy/fullchain-scale-test.mjs ai-indexer parser-svc upload-worker \
  object-worker media-worker maintenance-worker search-indexer outbox-relay
```

- API 从 2 扩到 3，缩到 1，再恢复 2。新 API 接受已有账户 token，创建的目录可通过其他副本看到。
- AI query 从 1 扩到 2，再缩回 1。检查 etcd 注册数量；通过每实例 gRPC 指标证明新副本处理 Search 和 Ask。4 个问答请求尚未完成时执行缩容，校验全部返回真实文档口令和引用。
- AI indexer 从 1 扩到 2，观测新副本存在运行中的真实索引任务后缩回 1，等待文件最终索引成功。
- 解析服务从 1 扩到 2，再缩回 1。检查 etcd 注册、每实例真实 ParseDocument 计数，以及缩容后的新文档处理。
- 各 worker 增加一个副本，检查 RabbitMQ 对应队列的 consumer 数量，发带唯一 ID 的 `system:ping` 验证新副本确实消费并持久化成功回执；同时执行真实上传、文件校验、图片缩略图或索引投影流程。队列验证与业务流程分别记录，ping 不代替业务验证。
- 上传、维护和 relay 的原副本数为 2，执行 `2 → 3 → 1 → 2`；其他单副本角色执行 `1 → 2 → 1`。缩容期间继续提交文件，随后验证新任务仍可完成。
- 持续通过 Web 访问目录、分享列表和受分享校验保护的文件流，并比对下载 SHA256。记录所有失败，不使用自动重试隐藏探针失败。
- 采集 PostgreSQL 连接数、主机内存、Docker 退出和 OOM 事件。最终检查副本数、镜像 ID、健康状态、无关容器 ID 均恢复或保持基线。

## 证据与结果

结果以 `artifacts/scaling-20261106/` 内 JSON 为准。账户和 token 仅写入忽略目录下权限为 0600 的 `.env.fixtures.json`，不要公开整个证据目录。

初次 API 测试使用了不存在的 `GET /folders/{id}` 校验路径，返回受控的 `404 route.not_found`，测试自动恢复 2 个 API。已改为真实目录列表接口后完整重跑；首轮证据保留在 `online/`，不作为通过结果。

最终 10/10 类服务均通过，逐项结果如下：

| 服务 | 实际副本变化 | 业务证据 |
| --- | --- | --- |
| API | 2 → 3 → 1 → 2 | 新 API 接受同一账户 token，写入目录跨副本可见；缩容期间 4 个新文件完成上传及索引 |
| ai-query | 1 → 2 → 1 | 新副本处理 4 次 Search、1 次 Ask；缩容开始时仍有 4 个未完成 Ask，最终全部成功且引用正确 |
| ai-indexer | 1 → 2 → 1 | 新副本消费任务；缩容时观测到 2 个运行中任务；同期 3 个新文件最终索引成功 |
| parser-svc | 1 → 2 → 1 | 新解析副本实际执行 ParseDocument；缩容期间及缩容后文档均完成处理 |
| upload-worker | 2 → 3 → 1 → 2 | consumer 随副本变化，新副本消费并写成功回执；缩容期间 4 个文件完成上传、校验及索引 |
| object-worker | 1 → 2 → 1 | 新消费者执行任务；真实上传完整性校验通过，下载 SHA256 一致 |
| media-worker | 1 → 2 → 1 | 新消费者执行任务；扩缩容各阶段的有效 PNG 均生成 ready 缩略图，原文件下载一致 |
| maintenance-worker | 2 → 3 → 1 → 2 | 新消费者执行任务；文件事件完成处理，最终配额、预留、引用审计无差异 |
| search-indexer | 1 → 2 → 1 | 新消费者执行任务；全部事件消费后 PG/ES/Qdrant 对账一致 |
| outbox-relay | 2 → 3 → 1 → 2 | 多副本及缩为单副本时新写入均完成派发，最终 Outbox 和消费回执收敛 |

通过的两份完整记录为 `online-verified/scaling-results.json`（2/2）和 `workers/scaling-results.json`（8/8），合并结果为 `scaling-summary.json`：

- 持续探针 671 轮，失败 0。每轮校验目录、分享列表和受保护文件下载内容。
- 6 类队列合计发送 144 个唯一 ping ID，全部收到持久化成功回执；每个新增消费者都有实际处理计数。
- PostgreSQL 采样连接数峰值 31；主机可用内存最低约 1626 MiB。配置上限与实测使用量分别报告，不把它们混为一谈。
- Docker 未记录 OOM，所有退出副本的退出码均为 0，没有强制杀死副本。
- 扩缩容产生的测试文件中，SQL 检查 74 个 AI 文档均为 indexed，无失败状态；包含初轮测试留下的探针文件。

恢复后重新执行了核心回归 12/12、分享专项 10/10、一致性审计 2/2，记录在 `regression/`。最终 167 个有效文件、118 个有效 chunk 与 PostgreSQL 完全一致，ES 的内容/版本/元数据和 Qdrant 的 1024 维向量无差异；配额、预留和对象引用差异为 0，队列无积压、终态失败为 0。

`final-state.json` 于 `2026-11-06T12:40:43Z` 确认：恢复原有 28 个运行容器；API/relay/upload/maintenance 各 2，其余本次测试的角色各 1；已有健康检查全部 healthy，Web 三个探针均 200；etcd 的 query/parser 注册各 1。所有镜像 ID 保持基线，无关容器 ID 未改变。ES/Qdrant、降级、AI 拆分、远程解析和 rerank 配置均保持开启。

本轮只新增可重复执行的测试脚本、报告及对应的文档忽略例外，没有修改业务代码或持久运行配置。

## 连接预算

当前应用连接池上限合计 74，另有 2 个 relay LISTEN 连接，PostgreSQL `max_connections=100`。逐类扩容时最大配置为 API 扩到 3 个，应用池加 LISTEN 合计 86。脚本要求另外保留超级用户和探针连接空间，避免同时扩展所有服务。

这次使用功能测试流量，不能由连接未耗尽推导生产容量。长期增加副本时还需重新分配连接池、运维余量和模型供应商预算。
