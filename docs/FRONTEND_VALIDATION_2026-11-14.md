# 前端检查与修复记录（2026-11-14）

本轮检查覆盖请求处理、页面状态、交互、UI 与响应式样式。正常流程通过；其余修复保留。用户已决定：上传创建期间的取消竞态本轮只记录，取消语义后续单独处理；问答失败后恢复输入的改动回退，恢复提交后清空、失败只提示错误的原行为。

## 验证环境与边界

- Node.js 22.23.0，React 18，Ant Design 5，Chromium headless。
- 原有测试栈入口 `http://127.0.0.1:24000`。运行中的前端 bundle 与工作区原有构建不同，分别记录，未把旧页面结果当作当前源码验证。
- 前期验证时，修复后的源码执行 `npm run build`，在 `http://127.0.0.1:24010` 临时提供构建产物，`/api`、`/storage` 代理到原有测试栈。该阶段没有替换运行中的 Docker 前端；后续按用户指令构建部署，见文末。
- 真实联调使用本轮独立注册的测试账号与文件，未使用用户账号或既有文件。复现用账号和测试数据保留在测试栈中，没有执行全局清理。
- 桌面与窄屏检查使用 320、390、768、1440 像素视口；窄屏是 Chromium 视口模拟，不等同于真机或 Safari/Firefox 验证。浏览器主用例设置减少动画。
- 文件上传、存储直传、下载 SHA-256、目录操作、分享、文件名搜索均使用真实后端。搜索乱序、问答成功渲染与错误、部分 HTTP 错误使用浏览器响应注入；未验证真实模型回答质量、容量或高可用。

## 已修复的问题

| 问题 | 触发与影响 | 修改及证据 |
| --- | --- | --- |
| 搜索旧响应覆盖新结果 | 连续按回车提交两次，先返回的新结果被后返回的旧请求覆盖 | `AIWorkspace.js`、`services/ai.js`：取消旧请求，忽略已取消的响应，离页取消请求；回归测试与浏览器控制响应顺序验证 |
| 上传任务被初始化覆盖 | 活跃上传列表较慢，此时新建上传；列表响应返回后任务从页面消失，成功或失败响应都能触发 | `TransferContext.js`：恢复任务与当前任务合并，按本地 ID 和服务端会话 ID 去重；单测覆盖成功和失败，浏览器覆盖真实上传完成后晚到的初始化响应 |
| 文件行误处理按钮的键盘事件 | 在目录管理按钮按 Enter 会进入目录；文件管理按钮会打开摘要 | `Drive.js`：行键盘处理仅接受行自身事件；单测与浏览器验证 |
| 桌面不支持摘要的文件无法用键盘访问操作 | `.bin` 等文件行不能获得焦点，操作按钮又默认隐藏 | `Drive.js`：这类文件行也可获得焦点，从而显示操作按钮；浏览器确认修复前无可见按钮且不可聚焦，修复后可聚焦 |
| 连续回车重复提交创建和重命名 | 输入框仍能在请求处理中提交，发出两次相同修改 | `Drive.js`：创建和重命名检查提交中状态；两条回归用例修复前都发送两次、修复后各一次 |
| 长文本撑破窄屏布局 | 220 字符连续文件名把搜索卡片撑到 2281px，外层裁掉右侧；问答与引用也需要限制长文本 | `AIWorkspace.js`、`AIAsk.js`：允许长词换行，约束卡片子项与引用宽度；浏览器截图和内容宽度测量验证 |
| 登录彻底失效后停留在受保护页面 | 根目录等请求 401，刷新凭据也 401，页面只显示错误，仍保持登录后的导航 | `services/api.js`、`AuthContext.js`：刷新确认 401 时清除令牌与当前用户，由路由回到登录页；单测及浏览器验证。502/503 不清除用户 |
| 刷新时丢失 HTTP 错误类型 | refresh 返回 HTML 502，被错误改写成浏览器网络故障，丢失状态码 | `services/api.js`：保留已解析的 HTTP 状态和中文提示；回归验证 502/503，以及并发 401 仅刷新一次 |

同时区分搜索的初始、检索中、没有匹配结果三种空态，避免搜索完成后仍提示“输入一句话开始检索”。

## 自动化测试稳定性

- 初始 18 个测试文件、101 项测试中，97 项通过，4 项超过默认 5 秒超时。扩大单用例超时后，相关原有用例通过。
- 中间一次同时执行构建和浏览器的全量测试还出现了 3 项不稳定失败，不能将该次记录视为通过。
- 修正 `setupTests.js` 的媒体查询夹具：原实现同时把 `prefers-reduced-motion: reduce` 与 `no-preference` 判为真，导致测试里仍运行入场动画。现在只匹配 `reduce`。
- Testing Library 异步等待设为 5 秒，以适应当前主机上的组件渲染和轮询。
- 分享期限单测使用固定时钟验证“保存时间加七天”，去除依赖实际渲染必须小于一秒的断言。
- 最终验证结果见下文；原始过程日志保留在 `artifacts/frontend-audit-2026-11-14/`。

## 问答行为回退前的完整验证结果

下表记录问答行为回退前的构建与测试结果。浏览器历史结果中的“失败后恢复问题”已被用户指定的原行为取代，不作为当前预期。

| 检查 | 实际结果 |
| --- | --- |
| `npm run lint`（直接执行，避开命令包装器） | 通过，0 errors / 0 warnings |
| `npm run build` | `Compiled successfully`；`main.629a4bcb.js` |
| 全量 Jest | 20 个测试文件、118 项测试全部通过，85.257 秒 |
| 当前构建浏览器主流程 | 18 项通过，0 个未捕获 JavaScript 错误 |
| 请求边界验证 | 7 项通过；另有 1 项取消竞态复现，用户决定延期处理；0 个未捕获 JavaScript 错误 |
| 真实分享与分片上传 | 7 项通过，0 个未捕获 JavaScript 错误 |
| `git diff --check` | 通过 |

浏览器共 32 项检查通过。请求边界脚本的 `DEFECTS_FOUND` 明确指向尚未处理的取消竞态，不代表该脚本全部通过。

全量测试的实际命令（在 `web/` 下）：

```bash
CI=true npm test -- --runInBand --watchAll=false --testTimeout=20000 \
  --json --outputFile=../artifacts/frontend-audit-2026-11-14/jest-results.json
```

构建、主要浏览器验证完成后，再单独执行最终全量 Jest，避免同时运行构建给本机造成额外负载。

## 用户指定回退：问答失败后恢复输入

用户决定恢复原方案：提交时立即清空输入框；请求失败时仅提示错误，不自动恢复已提交的问题。等待期间输入的新草稿继续保留。

已移除 `AIAsk.js` 中失败分支的 `setQuestion(current => current || value)`，同步调整已有回归用例及浏览器脚本的预期。问答正文与引用的窄屏样式修复保留。

回退后执行 `CI=true npm test -- --runInBand --watchAll=false --testTimeout=20000 src/pages/AIAsk.test.js`，4 项测试全部通过（含失败后保持清空、保留等待期间新草稿）；两份相关源码与测试文件的 ESLint 检查、`git diff --check` 均通过。日志为 `ask-revert-tests.log`、`ask-revert-lint.log`。回退当时未部署，也未重新构建或重跑浏览器预览；后续部署包含该回退。

## 已决定延期：创建上传期间的取消竞态

用户决定（2026-11-14）：“本轮先记录，上传取消语义以后单独处理”。本轮保留复现证据与现有取消逻辑，后续单独确定语义并实施修复。

稳定复现步骤：

1. 选择新文件，让 `POST /api/v1/uploads` 在服务端成功创建会话，但暂缓向浏览器返回响应。
2. 在传输页点击取消。此时前端还不知道会话 ID，显示“已取消”。
3. 放行创建响应，查询真实的活跃上传列表；会话仍为 `created`，没有发送 DELETE 取消请求。

复现证据见 `edges/request-edge-results.json`：记录了独立测试账号的会话 ID、服务端状态与取消请求数量 0。问题可能使预留空间和 multipart 会话持续存活到后续过期回收。

这不适合仅在请求上加 AbortSignal：浏览器中止请求不代表服务端事务回滚，而且同一 Create 请求也可能返回已经完成的秒传。以下方案供后续讨论，尚未选定：

- 建议：尚未完成则收到会话后补发取消，并在确认前显示取消中；若服务端已完成或秒传成功，则如实显示完成，不自动删除文件。
- 严格取消：点击取消必须不保留文件，需要进一步设计服务端取消与完成、秒传之间的协调协议。

该竞态的生产逻辑按用户决定保持原样，其余修复已完成验证。

## 本地证据

目录：`artifacts/frontend-audit-2026-11-14/`（按仓库约定忽略，不入库）。

- `baseline/browser-results.json`：原运行页面的正常流程与缺陷复现；长文件名用实际卡片宽度识别，不能只看 document.scrollWidth，因为外层会裁剪。
- `current/browser-results.json`：修复后构建的页面回归与截图。
- `edges/request-edge-results-before-auth.json`：会话失效修复前的边界证据。
- `edges/request-edge-results.json`：请求异常、初始化竞态、真实文件名搜索、长回答与取消竞态。
- `sharing-upload/results.json`：分片暂停恢复、跨账号分享导入、期限修改、撤销、删除源文件后的引用行为。
- `regressions-before.log`、`ai-regressions-before.log`、`duplicate-submit-before.log`：修复前的失败断言。
- `jest-results.json`、`jest-final.log`、`lint-final.log`、`build.log`：最终检查输出。
- `browser-audit.mjs`、`request-edges.mjs`、`sharing-upload.mjs`、`preview-server.mjs`：本轮浏览器验证脚本，使用此主机已安装的 Playwright，不引入仓库依赖。

前端修复、回归单测和本报告保留在工作区，未自动提交 Git。本轮启动的 `:24010` 临时预览服务在验证完成后停止。

## 后续构建与部署

用户随后指示“构建并重新拉起项目”。执行 `APP_IMAGE_TAG=frontend-audit-r1 ./deploy/build.sh web`，生产构建成功，新 bundle 为 `main.d64929d4.js`。镜像 `bx-yunpan-web:frontend-audit-r1` 已替换 `bx-fullchain-20261106` 项目的 Web 容器，入口仍为 `http://127.0.0.1:24000`。

沿用运行中容器标签记录的环境文件和全部 Compose 覆盖配置，追加 `artifacts/frontend-audit-deploy/compose.yaml`，通过 `up -d --no-deps --no-build --force-recreate --wait web` 重新创建 Web。其余 23 个项目容器的 ID 和启动时间均未改变。

实际部署检查：

- Web 容器 healthy，Nginx 配置校验通过。
- `/healthz`、`/readyz`、`/storage-healthz` 均返回 HTTP 200；两个 API 副本分别通过 readiness。
- HTTP 提供的新 bundle 已确认从 `main.3fcf58ae.js` 切换到 `main.d64929d4.js`。
- Chromium 未登录访问受保护路由能够进入登录页；1440px、390px 登录布局无页面横向溢出；检查期间无未捕获 JavaScript 错误和 HTTP 5xx。

本次部署后仅执行上述 smoke，未重新执行此前 32 项完整浏览器流程或全量 Jest。构建、启动、前后容器快照和检查结果保存在 `artifacts/frontend-audit-deploy/`；`deployment.json` 记录旧镜像及启动配置，可用于回退。
