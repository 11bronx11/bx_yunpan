# 异步验证入口

这些工具只用于专用测试资源，不读取 `deploy/.env`，不部署业务栈，也不删除已有数据库、bucket 或卷。

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
