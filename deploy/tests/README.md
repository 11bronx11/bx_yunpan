# 异步验证入口

## 已部署服务的最终黑盒验收

`final-acceptance.mjs` 对实际 `http://127.0.0.1:3000` 发送用户 HTTP 请求，创建带 `final_` 前缀的专属用户，执行上传/下载/分享/AI/竞态、32 并发尖刺与 600 秒浸泡，然后只读核对业务、控制和 AI 数据库。它使用现有 `bx-yunpan` Docker 服务；不创建隔离部署，不切换协议或修改配置，不清理测试数据。请只在允许测试账号、有限并发和模型调用的环境运行。

```bash
FINAL_OUTPUT=artifacts/final-acceptance-<唯一标识> node deploy/tests/final-acceptance.mjs
# private-fixture.json 出现后，可在另一个终端执行真实浏览器验收
node deploy/tests/final-browser.mjs artifacts/final-acceptance-<唯一标识>

# 独立重跑完整浸泡：重新登录，期间两次轮换 refresh cookie；覆盖输出前先保留旧证据
FINAL_OUTPUT=artifacts/final-acceptance-<标识> node deploy/tests/final-soak.mjs
# 所有写入停止后，核对该批用户拥有过的全部领域数据
FINAL_OUTPUT=artifacts/final-acceptance-<标识> node deploy/tests/final-supplement.mjs --reconcile
python3 deploy/tests/final-storage-audit.py artifacts/final-acceptance-<标识>
python3 deploy/tests/final-index-audit.py artifacts/final-acceptance-<标识>
python3 deploy/tests/final-broker-audit.py artifacts/final-acceptance-<标识>
python3 deploy/tests/final-intent-audit.py artifacts/final-acceptance-<标识>
```

Node 22+；Python 审计使用 boto3、requests。浏览器可用 `FINAL_PLAYWRIGHT_MODULE` 和 `FINAL_CHROMIUM_EXECUTABLE` 覆盖本机运行器。fixture 含专属测试登录凭据，输出目录权限 0700。`final-summarize.py` 专用于本次保留的分阶段证据，执行严格检查后生成根目录验收报告；不会把原始失败改写成成功。源码对应报告为 `FINAL_BLACKBOX_ACCEPTANCE_2026-11-20.md`。

### canonical.ensure 失败意图回归与定点修复

`intent-fix-e2e.mjs` 创建独立测试用户，通过真实 HTTP 做 4 轮 × 4 个同 owner、同内容并发上传。每轮要求一个成功、三个失败，并核对操作终态、失败码、完成时间、配额、引用及领域回执，下载成功文件校验 SHA-256。至少三个失败必须实际经过 canonical copy 提交，以免只覆盖复制之前的去重分支。

```bash
FINAL_OUTPUT=artifacts/intent-fix-<唯一标识> node deploy/tests/intent-fix-e2e.mjs
python3 deploy/tests/final-storage-audit.py artifacts/intent-fix-<唯一标识>
```

`repair-canonical-intents-20261120.py` 仅处理代码内固定的三条历史验收意图，不做全表修复。先验证控制任务 done/failed、result ID 和双摘要与领域回执一致、会话失败且预留为零、私有清理成功且 MinIO HEAD 返回 404，再在锁定操作/会话/回执的业务事务内补齐 failed、错误码和完成时间。完成时间取既有回执时间。默认只读；`--apply` 才写入，重复运行不重复更新。它不重开任务、不重新计算、不改配额或引用，也不删除字节。证据以每次运行的独立文件保留。

```bash
python3 deploy/tests/repair-canonical-intents-20261120.py --output artifacts/intent-repair-<标识>
python3 deploy/tests/repair-canonical-intents-20261120.py --apply --output artifacts/intent-repair-<标识>
```

## 专用资源验证

以下工具只用于专用测试资源，不读取 `deploy/.env`，不部署业务栈，也不删除已有数据库、bucket 或卷。

`async-check.py` 接收一个权限为 0600 的 JSON 对象，键为仓库测试实际使用的 `TEST_*` 环境变量。它核对真实数据库身份、业务 34/控制 2 的迁移历史，并在关闭会话只读设置后验证 reader 的 INSERT/UPDATE/DELETE/DDL 均被数据库权限拒绝。关键测试缺失、跳过、失败或命令超时均不能通过。原始退出码、JSON 测试事件、skip 和文件摘要写入输出目录。

业务库/控制库名必须明确包含 `test`、`async_` 或 `validation`。主机需提供 Go 1.25.13、Node 22、Docker、psql、Nginx；Go 由 `GOTOOLCHAIN=go1.25.13` 选择。Python 解析依赖来自 `backend/requirements-parser.txt`。进程验证额外使用 `minio==7.2.20`；浏览器使用独立 runner 的 `playwright@1.61.1`，不改前端依赖。

```bash
python3 -m venv artifacts/async-validation/venv
artifacts/async-validation/venv/bin/pip install -r backend/requirements-parser.txt minio==7.2.20
export PATH="$PWD/artifacts/async-validation/venv/bin:$PATH"

python3 deploy/tests/async-check.py --config /private/test-env.json --output artifacts/async-validation/my-run/regression --suite regression
python3 deploy/tests/async-check.py --config /private/test-env.json --output artifacts/async-validation/my-run/race --suite race
python3 deploy/tests/async-check.py --config /private/test-env.json --output artifacts/async-validation/my-run/web --suite web
python3 deploy/tests/async-check.py --config /private/test-env.json --output artifacts/async-validation/my-run/parser --suite parser
python3 deploy/tests/async-check.py --config /private/test-env.json --output artifacts/async-validation/my-run/static --suite static
```

用不同输出目录保留每次失败。后端套件和进程验证通过同一文件锁串行使用资源：同一 PG 实例的 xmin 跨数据库，暂停的事务会干扰另一个数据库中的 Relay。不要同时启动绕过此锁的故障测试。Redis 的 `CLIENT PAUSE` 只能对专用 Redis 开启；broker 重启/限额/网络故障也必须使用专用 broker，不能只凭 vhost 隔离判断安全。

当前四个已退役的旧执行链用例由名称精确排除；新的核心协议/核心冒烟必须实际通过。旧 taskqueue 传输兼容用例继续运行。子进程 helper、需要独立容器授权的旧稳定性/重启用例单列 skip，不计为通过。启用真实搜索和 parser 时，其关键测试同样必须实际出现并通过。

独立进程验证使用新数据库、新 bucket、独立队列前缀，启动双 API、双 control、双 committer 及四个计算角色，经过 Nginx 的 `/api` 和 `/storage`，输入全部走公开 HTTP。测试会暂停/强杀自己启动的进程，退出时恢复信号状态并停止这些进程；数据库、bucket 和队列现场保留。

```bash
# 在 backend 下；目录使用绝对路径，构建串行。
GOTOOLCHAIN=go1.25.13 go build -p=1 -o /absolute/test-binaries/ ./cmd/...

# 在仓库根目录；web-root 必须是本轮 npm run build 的生产构建。
python3 deploy/tests/async-process.py --config /private/test-env.json \
  --output artifacts/async-validation/my-run/process \
  --binaries /absolute/test-binaries --web-root "$PWD/web/build" --browser-wait 600
```

进程验证输出 `BROWSER_READY` 后，在另一个终端执行：

```bash
npm install --prefix artifacts/async-validation/browser-runner --no-audit --no-fund --ignore-scripts playwright@1.61.1
node artifacts/async-validation/browser-runner/node_modules/playwright/cli.js install chromium
ASYNC_PLAYWRIGHT_MODULE="$PWD/artifacts/async-validation/browser-runner/node_modules/playwright" \
  node deploy/tests/async-browser.cjs artifacts/async-validation/my-run/process
```

浏览器执行三次独立 context，包含 1440/390 视口、实际表单登录、新建目录、选文件上传、`/storage` PUT、下载 SHA、刷新和第二标签页；截图、trace 和结果保存在进程输出目录。trace、`private-env.json` 和 `browser-fixture.json` 包含测试登录/连接信息，不应公开发布。

这套入口覆盖有限的单机场景；不代表完整测试计划已通过，也不提供跨主机、基础设施集群、跨库 PITR 或容量认证。完整范围和首轮失败原因见本轮验证报告。


上传窗口核心冒烟：

`upload-smoke.py` 只执行上传相关的核心链路并等待浏览器，不运行现有 async-process.py 的故障演练。需要先启动专用 PG、MinIO、RabbitMQ 和 Redis，并提供已有 TEST_* 私有配置；会创建并保留新的业务/控制数据库、bucket 和测试账号，退出时只停止自己启动的应用与 Nginx 进程。输出中的 private-env.json、browser-fixture.json 和浏览器 trace 为私有验收材料。

```bash
# backend 下，串行构建本次实际使用的角色
GOTOOLCHAIN=go1.25.13 go build -p=1 -o /absolute/upload-smoke/binaries/ \
  ./cmd/api ./cmd/async-control ./cmd/committer ./cmd/upload-worker ./cmd/object-worker ./cmd/migrate

# 仓库根目录，web/build 应为当前源码的 npm run build 输出
PYTHONDONTWRITEBYTECODE=1 python3 deploy/tests/upload-smoke.py \
  --config /private/test-env.json --output /absolute/upload-smoke/run \
  --binaries /absolute/upload-smoke/binaries --web-root "$PWD/web/build"

# 输出 BROWSER_READY 后另一个终端运行
ASYNC_PLAYWRIGHT_MODULE=/absolute/browser-runner/node_modules/playwright \
  node deploy/tests/upload-browser.cjs /absolute/upload-smoke/run
```

浏览器覆盖单文件 4 片并发、暂停收尾批量确认、刷新后选择原文件只传缺失片、最终下载 SHA-256，以及 1440/390 视口截图。PUT 的短延迟仅用于稳定观察暂停操作。此入口不构成性能或深度故障验证。
