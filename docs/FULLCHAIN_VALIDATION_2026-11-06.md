# 全新构建与全链路验证（2026-11-06，北京时间）

本轮在当前工作区基础上构建独立测试环境 `bx-fullchain-20261106`。业务 `.env`、旧卷、旧镜像均保留；进入本轮时已有的 13 个修改/新增文件逐个 SHA256 核对，内容保留。

## 运行环境

- Web：<http://127.0.0.1:24000>，可在界面创建测试账户。
- Prometheus：<http://127.0.0.1:24909>；Grafana：<http://127.0.0.1:24001>；Jaeger：<http://127.0.0.1:26686>。
- PostgreSQL `26432`，Redis `27379`，MinIO `29100/29101`，RabbitMQ `26672/26673`，ES `29200`，Qdrant `29333`。端口均为测试栈专用，主要入口绑定回环地址。
- 私有配置：`deploy/.env.fullchain-20261106`，0600，已忽略。真实模型凭证、数据库密码和测试登录信息不写入本报告。
- 旧 `bx-rabbitmq-validation` 的 17 个运行容器已停止以释放内存，容器和数据卷保留；原清单为 `artifacts/fullchain-20261106/previous-running-containers.json`。不要在这台小内存主机上同时启动两个完整栈。

拓扑为 2 API、2 outbox-relay、2 upload-worker、2 maintenance-worker，以及独立 object-worker、media-worker、search-indexer、ai-query、ai-indexer、parser-svc。Redis cache/coordination 拆开，etcd 注册发现与监控追踪开启。combined worker/aisvc 不参与消费，避免与拆分角色重复运行。

开启真实 DashScope、1024 维 `qwen3.7-text-embedding`、`qwen-plus`、`qwen3-vl-plus`、`qwen3-rerank`、远端解析器、缓存、AI 账户限流、Outbox v2、通知唤醒与选主。索引写入 `postgres,es_qdrant`，读取 `es_qdrant`。

最终快照为 **2026-11-05 22:44 UTC 左右**，28 个容器运行，migration `31 / dirty=false`，3 个 Web/存储探针返回 200，15 个 Prometheus target 全部 up。严格检索验证完成后，最终保留 `SEARCH_EXTERNAL_FALLBACK=true`；外部索引正常时优先读取 ES/Qdrant。

连接池按运行副本实算：角色池上限 74 + relay LISTEN 2 + 运维预留 16 + migration 2 + PostgreSQL reserved 3 = 97，数据库 `max_connections=100`。这是预算配置与功能验证，不是吞吐量认证。

## 构建与代码修复

13 个目标 `migrate api worker aisvc parser-svc web outbox-relay object-worker media-worker maintenance-worker upload-worker search-indexer migrate-asynq` 完成串行构建，完整镜像版本为 `fullchain-20261106-r2`。之后只为第二项业务修复重新编译 API，运行 `fullchain-20261106-r3`；其余角色继续使用 r2。API 增量构建复用已完成的 Go 编译缓存，当前 API 源码重新编译成静态二进制，写入新镜像。

1. **AI 重新处理幂等缺陷**：唯一键冲突后，GORM 查询复用了带新主键的目标结构，第二个相同幂等请求可能返回 500。改为用空结构读取已存在的 Task。8 并发回归确认同一 Task、单条 Outbox，任务完成后的重放仍返回原任务。
2. **拆分后重新处理限流遗漏**：命令已迁回 API 本地事务，原 aisvc 限流不再覆盖该 HTTP 路径。API 现在使用相同 Redis owner/scope 预算，跨副本统一计数，Redis 故障时保留既有 fail-open 语义，并且不依赖 ai-query 存活。
3. recovery 测试的统计和清理条件漏列 `storage_operations` 的 operation ID，已修正；这项属于测试夹具修复。
4. Prometheus 增加 ai-query、ai-indexer 抓取。新增独立 Compose 覆盖、运行入口、API/浏览器/Go/索引审计脚本。

构建期间使用命令级 Go 镜像源覆盖解决下载 EOF，没有修改仓库默认源。没有执行 Docker prune 或删除旧卷。完整构建和临时数据库克隆挤压了磁盘；已将本轮通过检查的临时测试库以 0600 归档后释放在线副本，失败现场保留。ES 在此测试主机采用绝对剩余空间水位 `768mb/512mb/256mb`，生产环境需要独立规划。

## 验证结果

| 范围 | 结果与证据 |
| --- | --- |
| API 并发 | 最终 API r3 上 12/12，`core-r3.log`、`core-results.json` |
| 真实 AI | r2 完整 7/7；API r3 再验中 6/7，一次远端 Embedding 网络错误；检索专项连续 3 轮与真实重排均复验成功，见下文 |
| 共享限流 | API r3 上 2/2，`controls-r3.log`、`controls-results.json` |
| Chromium E2E | 最终部署再次 8/8，两个独立浏览器 context 同账户登录，另一个账户导入分享；无 page error、无 API 5xx，`browser-final.log`、`browser-results.json` |
| Go 全量 | 首轮 524 pass、1 fail、4 helper skip；失败项稳定环境连续 3 次通过。按测试名合并最新结果 525 pass、0 unresolved fail、4 helper skip |
| Go race | 首轮 380 pass、5 fail、4 helper skip；ES/中文链路失败项专项复验通过。按测试名合并最新结果 419 pass、0 unresolved fail、4 helper skip |
| Go 静态检查 | 全量 vet 通过；API 限流修复另有 race 回归及 vet 通过 |
| 前端 | lint 通过；14 suites / 51 tests 通过。初次负载下两个用例超时，使用 15s 测试超时重验通过，测试业务代码未改 |
| Python parser | 17 tests 通过 |
| 故障恢复 | 5/5，`faults-with-budget.log`、`faults-results.json` |
| PostgreSQL 回退 | 2/2，同时停止 ES/Qdrant 后搜索、授权隔离、来源问答均可用；恢复后检索正常，`fallback-results.json` |
| 数据与队列审计 | 2/2，配额/引用零差异、Outbox/receipt 收敛、42 个主/重试/死信队列积压为 0，`audit-results.json` |
| 外部索引逐条审计 | ES 56 个有效文件，ES/Qdrant 各 9 个有效 chunk，与 PostgreSQL 相同；ID、版本、内容、文件元数据和向量均无差异，`index-coverage.json` |

Go 结果来自完整套件加失败项定向复验，**没有将初次失败的执行记录改成成功**。`go-verification.json` 保留各次结果，`go-effective-results.json` 给出按测试名归并后的结论。4 个 skip 是供父测试启动的 subprocess helper，真实父测试已运行。每个 Go 包使用单独迁移数据库，避免全局 GC 扫描别的包的假对象夹具。

### 同账户、多设备与竞争的实测含义

- 8 个独立登录/refresh 会话，经两个 API 副本交错调用。相同 refresh token 并发轮换：1 成功、7 拒绝；重放撤销该设备 family，其他设备仍可刷新。
- 同名目录并发创建、相同版本 rename、同文件 rename/move/delete：单个 winner，其余返回受控冲突。互相移动目录重复 6 轮，没有生成目录环。
- 同一上传创建幂等键，8 设备得到一个 session；重复 part confirm/complete，持久操作每种仅一条。不同请求体复用该键返回 409。
- 同内容 6 上传竞争：1 完成，5 为 `upload.file_exists`，预留配额归零。**当前产品规则是同账户同 object 只保留一个有效 FileEntry**，不同文件名也不会生成多份。其他账户验证上传相同字节后共享 canonical object；删除原账户引用不影响对方下载。
- 同文件名不同 key 仅一个 pending；8 次 abort 只释放一次预留。complete/abort 重复 5 轮，收敛为 completed 或 aborted。
- 11 MiB 多 part 跨设备确认与完成，实际下载 SHA256 相同。
- 同一分享 8 请求并发导入只产生一个 entry；撤销分享后新访问与导入返回 404。
- 重新处理的真实账户预算：8 请求有 3 个 202、5 个 `429 ai.rate_limited`，接受的请求共用一个 Task 和一条 Outbox；36 个搜索请求为 30 个 200、6 个 429。

这里的 8 设备是 8 个独立认证会话，真实 UI 使用两个同账户浏览器 context，不代表在八台物理设备上测试。单设备退出撤销 refresh 会话；已签发 access JWT 继续有效至过期，默认 15 分钟，并非即时注销所有 access token。

### AI、ES/Qdrant 与网络失败

真实上传、worker 校验、远端解析、Embedding、摘要、索引投影、检索、问答引用均有运行证据。GB18030、UTF16、DOCX、PDF 解析与检索通过；图片真实缩略图和 vision 索引通过。初用旧 `qwen-vl-plus` 被服务商拒绝，测试配置改成预检和实际索引均成功的 `qwen3-vl-plus`。

严格阶段关闭 `SEARCH_EXTERNAL_FALLBACK`，验证名称、ES 全文、Qdrant 语义和混合检索。检索专项连续三轮成功；多文档问答实际调用 `qwen3-rerank`，5 个候选返回 5 个重排结果，追踪无错误，回答包含来源事实及引用。空白第三账户无法检索或引用其他账户的文档。

API r3 的一次语义搜索在 gRPC 三次尝试后返回受控 `503 ai.unavailable`。Jaeger 定位为远端 `/embeddings` 的两次 EOF 和一次 TLS handshake timeout；后续专项检索成功。证据为 `provider-transient-error.json`、`ai-r3.log`、`retrieval-results.json`。这说明网络与模型服务仍可能带来可观察的暂时不可用，本轮没有把该失败隐藏成一次全绿执行。

### 故障恢复与最终一致性

5 组故障均恢复成功：RabbitMQ 停机时 API 写入保存在 Outbox；两个 upload-worker 停机时上传保持 initializing，启动后推进；单个 API 重启不影响另一副本的会话和文件访问；ai-query 停机时普通网盘可用、搜索返回受控 503，本地重新处理命令仍能由 ai-indexer 完成；ES/Qdrant 同时停机时严格搜索返回 503，恢复后重新命中。恢复探针使用真实 API 健康路径、重新发现动态端口，并在账户限流 429 时等待预算窗口。

回退开启后再次同时停止 ES/Qdrant，全文、语义、混合检索走 PostgreSQL 可检索已授权资料，真实问答保留引用；第三账户结果为空。两套索引恢复后正常运行。最终状态继续开启回退，严格依赖外部索引的证据保留在前一阶段。

最终数据库审计：7 个 async Task 均 succeeded；`task_failures=0`，未发布 delivery 为 0，缺失成功消费 receipt 为 0，预留配额总和为 0，引用与配额差异均为 0。42 个队列积压为 0，6 个业务主队列都有 consumer。上传竞争测试会留下受控的失败 session，不应与异步任务终态失败混为一谈。

`index-coverage.json` 直接读取真实 ES/Qdrant 数据，与 PostgreSQL 的 active document version、有效 FileEntry 逐 ID 比较。有效文件 56、有效 chunk 9；没有遗漏或额外有效记录。ES 内容/版本、文件 owner/folder/name/version/object 元数据一致；Qdrant 1024 维向量与 PG 向量余弦相似度均不低于 0.999999。历史版本与 tombstone 不计作有效 chunk。

Jaeger 保存了跨 7 类服务、104 spans 的任务追踪示例，覆盖上传恢复、对象校验、媒体、解析、Embedding/LLM 和搜索投影。真实重排调用的 trace 为 `e8f81ee77d771d944cc8543d9abe5b23`。安全摘录为 `tracing-evidence.json`、`retrieval-results.json`。运行镜像与资源快照见 `image-manifest.json`、`runtime-final.json`。

## 复跑

在仓库根目录执行，使用当前私有测试配置，不替换业务 `.env`：

```bash
bash deploy/fullchain.sh ps
SEARCH_EXTERNAL_FALLBACK=false bash deploy/fullchain.sh up
bash deploy/fullchain.sh core
bash deploy/fullchain.sh ai
bash deploy/fullchain.sh retrieval
bash deploy/fullchain.sh controls
node deploy/fullchain-browser.mjs
bash deploy/fullchain.sh faults
SEARCH_EXTERNAL_FALLBACK=true bash deploy/fullchain.sh up
bash deploy/fullchain.sh fallback
bash deploy/fullchain.sh audit
python3 deploy/fullchain-index-check.py
```

顺序执行；core 会重建当前测试 fixture 账户信息，ai 会增加文档。不要在 Go 外部依赖集成测试运行时重建栈或注入服务故障。普通 Go 验证为 `python3 deploy/fullchain-go-check.py`，需要本机 Go 和已安装解析器 Python 环境；其结果文件保留初始执行与复验历史。

只停测试栈：`bash deploy/fullchain.sh stop`。没有自动提交本轮改动。`artifacts/fullchain-20261106/` 是本地证据目录，包含忽略和私有文件；不要整体提交或公开。

## 边界与后续条件

这是 4 CPU / 约 5.2 GiB RAM 的单机独立环境。ES 单节点 yellow 来自未分配副本，主分片可用。没有进行跨主机分区、PG 主从切换、ES/Qdrant 多节点副本失效或持续容量认证。测试支持上述并发与故障窗口内的行为结论，不构成任意设备数/任意并发的保证。

构建后磁盘仅余约 0.9 GiB，需要在长时间压测或下一次完整冷构建之前扩容，或由用户审阅旧数据与镜像清理清单。本轮保留旧镜像、旧卷与原工作区数据。
