# 已部署链路最终黑盒验收（2026-11-20）

更新：**三条失败意图已修复，修复版本已部署，真实并发及新的完整 10 分钟浸泡、最终数据收敛审计通过**。全库仍保留两条验收前已有的历史引用异常。以下保留修复前的原始验收记录；修复、部署及复验见文末「失败意图修复与复验」。原始 FAIL 证据没有覆盖。

初次验收结论：**最终完整数据验收未完全通过**。主要用户链路、完整 10 分钟持续负载、业务引用/配额/对象字节、控制结果与回执均通过；但本轮并发同内容上传留下 **3 条已经失败终结、旧操作意图仍为 pending 的 canonical.ensure 记录**。此外全库有 2 条验收前已存在的历史引用异常。初次验收未修改或清理这些数据，也未修改业务实现。

## 环境与执行边界

- 入口 `http://127.0.0.1:3000`，使用实际 Web → Nginx → API → 独立业务/控制/AI PG、RabbitMQ、MinIO、parser、etcd、ES、Qdrant。没有 mock HTTP、fake provider 或直接 SQL 造业务结果。
- 实际 `ASYNC_PROTOCOL_VERSION=2`；三库迁移 37 / 6 / 12，均 dirty=false。当前 Go/Web 镜像 `ask-tiers-4aaf7ea-20261119-224649`，parser 镜像 `release-7c2ebdc-20261119-151300`；完整镜像 ID 和启动时间见 `db-baseline.json` / `db-post-soak.json`。
- 使用 3 个 `final_...` 专属用户；仅通过公开 HTTP 创建、修改、删除这些用户的数据。数据库与对象审计均只读。未改业务实现、部署配置、镜像、协议、迁移和现有用户数据；测试数据与证据保留。
- 单机有限并发；没有执行跨主机故障、PG 切换、v3 协议切换或 24 小时后的实际物理 GC。

## 用户请求、预期与结果

| 链路 | 预期及实测 |
|---|---|
| 注册 / 登录 / 根目录 / 配额 | 201 / 200；匿名保护接口 401 |
| 同名目录 8 并发创建 / 乐观版本重命名 | 各恰好一个成功，其余 409 |
| 20 MiB + 123 B，3 片倒序直传 | 创建 201，presign/PUT/confirm 200；缺片 complete 409；补全后 202 → completed；SHA-256 一致 |
| 幂等与分片冲突 | 同 key 4 次仅一个会话；不同请求同 key 409；重复确认成功，冲突 ETag 409；完成重放均 202 |
| 错误哈希 / 重复取消 | failed / aborted；终态预留归零，原对象不受影响 |
| 文本 / CSV / 图片 | 文本与 CSV 真实解析到 askable；图片缩略图 ready；下载字节一致 |
| 分享与权限 | 跨 owner 文件、作业 404；8 并发幂等导入一个 entry；Range 206；撤销及真实等待到期后旧 token 404；导入文件保留 |
| 同内容与引用 | 跨 owner 复用 canonical；同 owner 4 并发恰好一个完成，其余 failed / upload.commit_precondition_failed；对应 3 条旧意图仍 pending，严格全表收敛不通过；删除单个引用后其他用户下载正常 |
| complete / abort 竞态 | 3 次均进入有效终态，预留无泄漏 |
| AI name / fulltext / semantic / hybrid / Ask | 本轮全部真实成功；单文件和全库 Ask 返回版本化引用，越权 scope 409，空选择 400 |
| AI summary 重建 | 带幂等键 202，同 key 重放同 job；最终 succeeded，发布版本与回执可核对 |
| 浏览器 | 真实登录、建目录、选文件、storage PUT、刷新、浏览器下载哈希、第二标签页、390px 视口共 6 项通过；无 pageerror |
| 最后引用删除 | 引用为 0，访问 404；object.authorize_delete 按零引用时刻 + 24h 调度，宽限内保留字节符合配置 |

真实模型的网络、余额失败原本允许单列，但本轮所测查询、问答、摘要和投影样本实际成功。未知二进制格式不要求产生 AI 内容；这类任务按领域结论收尾。

## 尖刺与完整浸泡

- 尖刺：32 个并发用户循环，共 320 次请求，256 次 200、64 次 `503 service.overloaded`；拒绝携带 `Retry-After: 1`。P95 306.84 ms / P99 420.75 ms。等待 1.1 秒后连续 10 次目录请求全部 200，合计 204 ms。
- 8 个并发真实上传均 completed，逐个下载 SHA-256 一致；最长端到端 3547 ms。
- 有效浸泡：北京时间 **2026-11-20 07:44:31 至 2026-11-20 07:54:31**，实际 600.131 秒。6 个读循环全程保活，每循环次数 [3494, 3499, 3480, 3491, 3484, 3477]，另有目录写入 57 次与 19 次真实上传/下载；会话按正常客户端流程完成 2 次 refresh cookie 轮换。
- 共 21235 次记录请求，35.38 请求/秒，P95 82.77 ms、P99 201.24 ms。状态分布：`{"200": 21115, "201": 38, "409:ai.scope_changed": 32, "204": 19, "202": 19, "503:service.overloaded": 12}`。
- 并发修改期间的 `409 ai.scope_changed` 表示旧授权快照失效；`503 service.overloaded` 表示有界准入拒绝，均单独计数。没有未分类 HTTP 错误、网络超时或崩溃。各容器未重启、未 OOM，最终健康状态全部 healthy。

## 最终数据核对

结束持续写入后，核对全部 3 个测试账号拥有过的上传、逻辑文件、对象及其任务，包含浏览器和补验数据，而非仅核对脚本内存里的成功文件。

- 本轮事件 786 条全部进入控制 inbox；待处理到期任务和 running 任务为 0，stopped 为 0。
- 426 条核心结果、85 条 AI 结果与领域回执逐项比对，缺失/冲突为 0。
- 引用计数、completed 会话的 entry/object/owner、有效 entry 的对象 ready 状态、终态预留均一致；配额以 `user_quota_usage.occupied_bytes` 为权威，旧 users 计数字段不作为验收依据。
- AI source/text/summary/vector 活跃指针、父子片数量、向量数量、projection complete 的 expected/applied 数量全部一致。
- MinIO 65 个对象、25169027 字节逐个核验大小和 SHA-256；本轮 staging=0、未完成 multipart=0。
- 真实 ES 4 个活跃子片与 PG 内容/父片/版本一致；Qdrant 4 个活跃 1024 维向量与 PG 一致，cosine ≥ 0.999999。
- 当前协议 12 个队列 ready=0 / unacked=0，work 消费者均存在。
- 间隔 456.685 秒二次核对，业务/控制/AI 的 13 组数据无变化；这也确认下述 3 条旧意图没有自然终结。

| 用户 | 有效逻辑字节 | occupied_bytes | 会话预留 |
|---|---:|---:|---:|
| peer | 14421 | 14421 | 0 |
| owner | 22002393 | 22002393 | 0 |
| outsider | 3146097 | 3146097 | 0 |

未到期任务仍保留，不能以数据库任务总数非零判断未收敛：

| 类型 | 数量 | 最早执行时间（北京时间） |
|---|---:|---|
| `ai.artifact.gc` | 65 | 2026-11-28 08:00:00 |
| `multipart.sweep` | 7 | 2026-11-20 08:27:59 |
| `object.authorize_delete` | 2 | 2026-11-21 07:36:43 |
| `upload.expire` | 73 | 2026-11-21 07:27:52 |

## 本轮未收敛的旧操作意图

以下记录 available_at 已过、上传会话已经 failed、控制任务已经 done / failed，原始计算结果和领域回执均存在；但业务库 storage_operations 仍为 pending。3 个失败计算输出均已直接向 MinIO HEAD 确认不存在，因此本轮未发现对应字节泄漏，配额预留为 0。控制库是唯一执行权威，旧意图不会自行重跑；但旧意图的状态没有表达终结结论，不能将其写成全表数据已收敛。

| 意图 ID | 上传会话 | 意图状态 | 会话状态 | 控制状态 / 结论 |
|---|---|---|---|---|
| `d36fc7ed-e860-5f2f-b8bd-c8025548b849` | `01a1f62b-45cc-7258-b77e-a71c5b139665` | pending | failed | done / failed |
| `bd70a73d-b434-52be-bef9-7ddbb7d26109` | `01a1f62b-45e7-758e-ab73-69944f35503d` | pending | failed | done / failed |
| `9720b253-a9a5-5a1d-b1d7-8c1bd0c4e601` | `01a1f62b-4562-76c6-b2be-b5fcb454e2a0` | pending | failed | done / failed |

复现：同一 owner 并发创建 4 个不同文件名、相同 SHA-256/大小的上传，在成功建 entry 之前全部完成分片并提交。1 个会话成功，3 个在提交时发现 owner 已有同对象 entry，失败收尾。当前源码 `backend/internal/asyncjobs/committer.go` 的 ErrNameConflict 分支改走 out.Convergence；`backend/internal/upload/async_commit.go` 的收尾分支在 markIntent 之前返回，留下原 canonical.ensure 意图的 pending 状态。这个问题本轮已定位和留证，**尚未修复或重新部署**。

原始 dedup 用例最先因沿用旧 upload.file_exists 预期而失败；后续检查源码确认当前公开终态码为 upload.commit_precondition_failed，并进一步只读核对发现上述 3 条实质状态残留。没有仅通过放宽断言将该场景改判成功。独立 `intent-audit.json` / `final-intent-audit.py` 明确返回 FAIL。

## 历史遗留与验收脚本校正

以下 2 条异常在开始前已存在，结束时数量和身份未改变；不属于本轮数据，未修改：

| 对象 | bucket | reference_count | 实际活跃引用 | 创建时间（UTC） |
|---|---|---:|---:|---|
| `01a06d5e-6b38-789f-8944-61c4b84dc7be` | test | 1 | 0 | 2026-09-04T17:01:39.512583+00:00 |
| `01a06d2e-df8f-7de8-87f3-8d3666920ef8` | test | 1 | 0 | 2026-09-04T16:09:43.567928+00:00 |

首轮验收脚本有几处预期错误，原始失败文件均保留：重建漏幂等键导致 400、尖刺将准入 503 误判成错误、混合写入时未接纳 ai.scope_changed，以及误把 gc_eligible_at 当作最终删除时间。重建、尖刺、GC 均用修正后的请求/断言重新实测通过。首轮短浸泡因读循环退出而作废，明确停止本轮自有压测进程。

另一次整段浸泡虽达到 600 秒，但复用了已签发数分钟的 access token，末段达到 900 秒 TTL 后出现 697 次 `401 auth.unauthorized`；该次明确判为 FAIL，保留于 `soak-token-expiry-results.json`，没有计入有效验收。最终重新真实登录并增加主动会话轮换，从零完整执行上述 600 秒，原始 runner 状态为 **PASS**，unexpected=0。所有原始 JSON 均保留。浏览器早期定位器及上传跳转页的脚本错误亦保留，最终浏览器验证为 PASS。

## 证据与复跑

原始材料：`artifacts/final-acceptance-20261120-live`，汇总 `acceptance-summary.json`。包括 `first-attempt-results.json`、`supplement-results.json`、`edges-results.json`、`edges-gc-corrected-results.json`、`browser-results.json`、`soak-results.json`、`reconciliation-results.json`、`db-baseline.json` / `db-post-soak.json`、`storage-audit.json`、`index-audit.json`、`broker-audit.json`、`intent-audit.json`、资源样本与浏览器截图。私有 fixture 包含测试登录凭据，目录权限 0700，勿公开发布。

复跑使用新增 `deploy/tests/final-*.mjs` / `final-*-audit.py`；主入口 `FINAL_OUTPUT=artifacts/<新目录> node deploy/tests/final-acceptance.mjs`，浏览器入口 `node deploy/tests/final-browser.mjs artifacts/<目录>`。后者可用 FINAL_PLAYWRIGHT_MODULE / FINAL_CHROMIUM_EXECUTABLE 指定本机浏览器运行器。库、bucket、卷及测试数据不会自动清理。相关脚本已完成 Node 语法、Python AST 和 git diff --check 检查；未改业务代码，因此未重跑后端全量单元测试。

## 失败意图修复与复验（2026-11-20，UTC）

修复后的本次范围结论为 **PASS_SCOPED_FIX**：真实 HTTP 的四轮同内容并发上传、三条历史记录定点修复、新的完整 600 秒浸泡及最终数据核对均通过。没有把两条既有历史异常改判为正常，也不代表跨主机高可用或容量认证。

- **代码**：上传失败收尾在释放会话预留、登记私有输出清理、写领域回执的同一事务内，将匹配会话的 pending/running 操作意图置为 failed，补齐有界错误码和完成时间，清除旧租约。已经 succeeded/failed/dead 的操作不被晚到失败覆盖。v2/v3 复用该领域逻辑；运行环境仍为 v2。
- **回归**：修改前，同一回归用例在 v2 和 v3 均能复现 pending 残留。修改后，专用业务/控制数据库、独立 RabbitMQ vhost、独立 MinIO bucket 下，asyncjobs/upload/storageops 三包共 110 项通过（含子测试），零跳过；上传业务竞态检测 13 项通过（含子测试），vet 通过。覆盖实际 committer 冲突回退、回执失败导致事务整体回滚、重复提交不重复结算和晚到失败保护成功意图。未跑后端全量套件。
- **部署**：四个共享该提交逻辑的核心 worker（upload/object/media/gc）已顺序更新到 `intent-fix-20261120-030328` 构建，均健康，保留各自原镜像的 rollback 标签。`deploy/.env` SHA-256 和全部 worker 运行环境与部署前一致，其他 20 个容器身份和启动时间未变。为保留唯一配置来源，配置中的旧镜像标签指向新镜像，同时保留独立 release 标签；以 `artifacts/intent-fix-20261120/deployment.json` 的实际镜像 ID 为准。
- **真实并发**：`intent-fix-e2e.mjs` 四轮 × 四个上传，合计 4 个 completed、12 个 failed；12 个 canonical.ensure 失败意图全部完成收尾，成功文件逐个下载 SHA-256 一致。170 次记录 HTTP 请求，两组断言均 PASS；业务配额/引用、控制结果与领域回执一致，无到期待处理任务。MinIO 四个对象共 131072 字节通过，staging/multipart 均为零。
- **历史三条**：先只读核验固定操作/会话/任务/result ID、摘要、failed 领域回执、零预留、成功清理及 MinIO 404，再在业务事务中仅补齐这三条意图的 failed、错误码和回执时间。实际更新 3 条；再次执行更新 0 条，证明幂等。控制任务、原始结果、领域回执、配额、引用和对象字节未重写。原测试用户与新增测试用户的 pending 终态 canonical 意图均为零。前述两条 9 月历史对象异常保留。
- **修复后浸泡**：UTC 03:05:24.229–03:15:24.438，实际 600.210 秒；6 个读循环分别执行 [3490, 3492, 3491, 3475, 3462, 3473] 次。21194 次请求、19 次真实上传/下载、57 次目录写操作、2 次 refresh cookie 轮换，unexpected=0。P95 86.48 ms、P99 198.82 ms；200=21082、201=38、204=19、202=19，另有符合契约的 32 次 ai.scope_changed 409、4 次 service.overloaded 503。运行角色无额外重启或 OOM，最终全部 healthy。
- **最终收敛**：覆盖原三个账号及新增竞态账号的全部领域数据。1146 条事件全部接管，655 条核心结果与 108 条 AI 结果均匹配领域回执；到期待处理任务、stopped 任务、终态上传对应未完成的 canonical.ensure/multipart.create/multipart.complete 意图均为零。88 个对象共 25611395 字节通过大小与 SHA-256 检查，staging=0、multipart=0；原 3 个加新增 12 个失败 canonical 私有输出逐个 HEAD，15/15 均 404。四个账号的配额、引用、终态预留、entry/object 身份和 AI 活跃版本均一致，真实 ES/Qdrant 核对通过，12 个当前协议队列 ready/unacked 均为零。未来调度的 expiry、sweep、延迟 GC 按配置保留。

修复材料独立保存在 `artifacts/intent-fix-20261120/`：汇总 `fix-summary.json`，以及 `reproduce.log`、`regression-complete-{summary.json,log}`、`race-{summary.json,log}`、`vet.log`、`deployment.json`、`repair/repair-*.json`、`failed-output-audit.json`、`live/intent-fix-e2e.json` 和新浸泡的 `soak/`。`deploy/config-check.sh` 亦实际通过。连接与测试凭据文件仅保存在权限受限的 artifacts 内，不应提交或公开。
