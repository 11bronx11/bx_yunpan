# 分享模块对齐与验证（2026-11-06）

## 功能与协议

- 云盘和搜索结果共用创建弹窗，确认后才创建。默认 7 天，可选 1 天、30 天、长期有效或自定义未来时间。Key 仅在创建时返回，后端仍只保存 HMAC，不将 Key 放进浏览器持久存储。
- 新增“我的分享”页面 `/shares`，桌面和移动导航均有入口。列表展示文件名、状态、有效期、创建时间，支持加载更多、刷新、修改有效期和确认取消。
- `GET /api/v1/shares` 默认 50 条、允许 1..100，按 `(created_at DESC, id DESC)` 游标分页；游标签名绑定账户。列表和详情补充文件名、大小、类型、可用性、状态和版本。
- `PATCH /api/v1/shares/{id}` 要求 `version` 和显式 `expires_at`；`null` 表示长期有效。旧版本返回 `409 share.version_conflict`。允许为已过期分享续期；已撤销分享不能恢复。更新通过事务推进 ACL epoch，写 `share.updated.v1` Outbox，并由搜索投影消费。
- 撤销使用行锁，重复或并发撤销返回 204，仅第一次推进版本、ACL epoch 和撤销事件；其他账户仍返回 404。数据库异常不会再一律伪装成不存在。
- 创建事务锁住有效源文件，防止与源文件删除交错产生可用分享。导入事务使用锁定后的当前文件名，延续原有配额、引用和导入幂等保证。
- 导入弹窗取消旧解析请求，防止旧 Key 的迟到响应被导入；对同一次解析结果的重试复用同一幂等键。导入失败后保留可重试状态。
- 导入配额不足返回 `429 quota.exceeded`，不再误报为文件重复；配额数据库异常保留为内部错误。失败事务不新增导入记录、扣减配额或增加对象引用。
- 公开内容接口返回受分享校验保护的相对下载/预览 URL，下载、HEAD、Range 均在 `/api/v1/public/shares/file` 校验当前过期与撤销状态，避免新签发的 S3 URL 在撤销后绕过检查。流式传输支持范围读取，不将整个文件读进内存；响应为 no-store，预览只允许安全 MIME，文本强制 text/plain。
- 文件流 URL 中的临时 token 不进入 nginx access log，该路径也不采集 URL tracing。OpenAPI 和生成模型同步更新。

## 行为边界

- Key 关闭后无法从服务端找回；需要新 Key 时，从文件重新创建分享。修改有效期保留原 Key。
- 取消和过期阻止后续访问、下载请求及导入，不回收已导入副本、已下载内容，也不能中止在校验通过后已经开始的响应。
- 上一版已经发出的 MinIO 预签名 URL 仍受其原有过期时间约束，新版入口不能撤回历史签名。
- 当前产品继续使用 Key 解析/导入流程，未增加匿名公开落地页或密码分享。
- 本轮不新增数据库迁移，现有 shares 表、版本字段和 owner/created 索引继续使用。

## 验证和环境

验证在独立 `bx-fullchain-20261106` 栈进行，2 个 API、2 个 relay 和真实 PostgreSQL、MinIO、RabbitMQ、Redis、ES/Qdrant。普通后端回归为每个包创建独立迁移数据库，防止异步维护扫描其他包夹具。

本轮证据在 `artifacts/sharing-20261106/`，不能整体公开（其中包含临时数据库归档）。现有 AI/上传改动以及业务 `.env`、旧镜像、数据卷保留。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| 后端全量回归（34 个测试包，真实依赖） | 535 个含子用例通过；4 个子进程 helper 入口跳过，由父测试实际调用 | `verified-go/go-verification.json` |
| 后端全量 vet | 通过 | 同上 |
| 最后配额修复后的分享 race，连续 3 轮 | 48 个含子用例通过，无跳过；另跑分享 vet 通过 | `quota-go/go-verification.json` |
| 前端全量 Jest | 17 suites / 61 tests 通过 | `checks/web-jest.log` |
| 前端 lint | 通过 | `checks/web-lint.log` |
| Python 解析器 | 17 tests 通过 | `checks/python-parsers.log` |
| 最终 r2 容器分享实测 | 10/10 组通过，89 次 API 请求 | `runtime/sharing-results.json` |
| 最终 r2 项目核心回归 | 12/12 组通过，含多设备、multipart、去重、配额、分享、版本冲突 | `runtime/core-results.json` |
| 真实模型/远程解析/检索回归 | AI 7/7，检索与重排 2/2，跨设备限流 2/2 | `runtime/{ai,retrieval,controls}-results.json` |
| 浏览器实测 | 10 项通过，3 个独立 context；页面异常 0、API 5xx 0；390px 页面无横向溢出 | `runtime/browser-results.json`、`runtime/sharing-mobile.png` |
| 依赖中断和恢复 | 5/5 组通过：RabbitMQ、upload-worker、单 API、ai-query、ES/Qdrant | `runtime/faults-results.json` |
| 恢复原搜索降级配置后验证 | 2/2 组通过，索引离线时授权检索与真实问答可用，越权命中为 0，索引恢复后检索正常 | `runtime/fallback-results.json` |
| 最终数据和队列审计 | 2/2 组通过：配额/预留/引用差异均为 0，6 类队列均有消费者且无积压，终态失败 0 | `runtime/audit-results.json` |
| 最终外部索引对账 | 83 个有效文件、43 个有效 chunk 与 PostgreSQL 完全一致；ES 内容/版本/元数据和 Qdrant 1024 维向量无差异 | `runtime/index-coverage.json` |
| 最终容器与入口 | 28 个容器运行，已有健康检查均 healthy；3 个 Web 探针均 200 | `runtime/deployment-final.json` |

后端 535 项全量回归完成后，追加了导入配额错误分类修复；该修复再跑分享真实数据库 race 3 轮及 vet，所有最终镜像也已重新编译。以上两个计数分别报告，不将专项重跑加算成另一次全量回归。

分享实测覆盖账户隔离、分页游标、真实字节下载/预览/HEAD/Range、失败配额回滚、8 路同键导入、8 路有效期修改（1 成功/7 个指定 409）、ACL 投影消费、8 路撤销仅 1 个事件、原 Key/token/已签发 URL 失效、过期续期、源文件删除和已导入副本存续。

首轮后端全量回归曾因磁盘不足失败：ES 磁盘水位拒绝写入、MinIO 空间保护，以及一次 ES 并发更新 409。安全清理后，重新执行的全量回归 535 项通过；原失败日志保留。ES 为排障临时添加的水位设置已移除，原 persistent 设置保留。分享实测首轮修正了测试脚本的索引配置名称（实际为 `SEARCH_INDEX_EPOCH`）；浏览器首轮修正了 Ant Design 下拉框内部输入元素被遮挡的自动化定位。故障套件一次在 4 组完成后的定时等待期间收到 SIGTERM，未产生最终结果，保留为 `faults-interrupted-*`；缩短不必要的整分钟等待后完整重跑 5/5 通过。最终结果均来自完整重跑。

清理仅移除 17 个无挂载、无 Compose 归属的失败构建容器、dangling 镜像及 pip/npm/golangci-lint 下载或检查缓存；未使用 `image prune -a`，未清理数据卷。已明确包含两个旧构建阶段缓存，后续构建已成功重新生成需要的产物。带标签运行/回退/基础镜像以及 Go 构建和模块缓存、浏览器均保留。清理后校验原有 28 个运行容器及其镜像未受损，用户已复核；记录见 `cleanup/`。

## 构建和启动

本轮使用专用增量构建，复用主机已有 Go/npm 依赖缓存，顺序编译 API、relay、search-indexer 和 Web。每次后端均编译当前源码生成新二进制，前端执行 CRA production build。后端复用旧运行镜像作为基础层，Web 复用 nginx 基础层；不是复用旧程序文件。

```bash
bash deploy/build-sharing.sh
bash deploy/sharing-up.sh
```

`compose.sharing.yaml` 只覆盖这 4 类镜像，其他角色沿用测试栈配置。后端基础镜像默认 `fullchain-20261106-r2`，最终输出默认 `sharing-20261106-r2`，arm64。本轮先构建 r1，再将配额修复同步进 r2 并部署。常规构建入口仍是 `deploy/build.sh`，它在 Dockerfile 内编译，同样默认允许使用缓存；本轮没有修改常规入口的行为。更新后的测试栈应使用 `sharing-up.sh`；单独调用原 `fullchain.sh up` 会采用原镜像配置。

| 镜像 | 最终镜像 ID（短） |
| --- | --- |
| `bx-yunpan-api:sharing-20261106-r2` | `8e9e4f47799c` |
| `bx-yunpan-outbox-relay:sharing-20261106-r2` | `11772ae6bc17` |
| `bx-yunpan-search-indexer:sharing-20261106-r2` | `19047474debb` |
| `bx-yunpan-web:sharing-20261106-r2` | `5390000a7899` |

完整镜像 ID 和构建模式见 `image-manifest.json`；构建、启动日志分别为 `build-r2.log`、`deploy-r2.log`。Web 地址 `http://127.0.0.1:24000`，分享管理为 `/shares`。业务 `.env` 未修改，未删除或重建数据卷，未新增数据库迁移。

最终检查时间为 `2026-11-06T11:28:54Z`：2 个 API、2 个 relay、search-indexer 和 Web 均采用上表最终镜像，其余角色保持原镜像。搜索降级已恢复原来的 `true`；ES 原 persistent 水位与任务前记录相同、transient 为空；项目磁盘可用约 17.3 GiB。故障测试停止过的服务均已恢复。

部署后检查：

```bash
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-test.mjs sharing
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-test.mjs core
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-test.mjs ai
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-test.mjs retrieval
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-test.mjs controls
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-test.mjs faults
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-test.mjs fallback
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-test.mjs audit
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime node deploy/fullchain-browser.mjs
FULLCHAIN_OUTPUT=artifacts/sharing-20261106/runtime python3 deploy/fullchain-index-check.py
```

AI/retrieval/faults 套件要求 `ai-query` 的 `SEARCH_EXTERNAL_FALLBACK=false`，以证明外部索引实际参与；fallback 套件要求它为 `true`。本轮仅在专用测试栈临时重建该服务切换配置，已恢复原来的 `true`。不要在未切换配置时直接把上述命令列表当成一次无条件执行的脚本。

本报告验证范围是单机测试栈的功能、并发边界和回归，不等同于跨主机容灾或容量认证。
