# 构建镜像保留规则

`deploy/build.sh` 依赖 Docker、Python 3.9+ 和 `flock`，继续按现有 CPU/内存预算串行构建。`deploy/up.sh` 的构建步骤自动使用这套规则。

- 构建前，给本轮目标已有的 `APP_IMAGE_TAG` 镜像加 `build-rollback-<完整镜像哈希>` 标签，同标签重建也能保留旧版本。相同镜像复用同一标签，`build-rollback-1/2` 两个标签记录最近两次不同的前置版本。
- 整批目标构建成功后，按每个 `bx-yunpan-*` 本地镜像仓库保留本轮标签和两个旧镜像 ID，优先保留上述回滚槽，再按创建时间补足。没有本轮标签的角色将最新版本视为当前版本。多个标签指向同一 ID 只算一个版本。
- 所有现存容器引用的镜像额外保留，包括已停止、仅创建的容器。带其他项目标签或项目命名空间以外 digest 的镜像不清理；兼容 containerd 为本地镜像自动生成的项目 digest。
- 三个 Dockerfile 的编译阶段标记 `io.bronx.yunpan.build-stage`，分别为 `go`、`parser-go`、`node`。每类保留最新一份无标签编译镜像及其父层，回收更早且没有容器引用的同类镜像。没有该标记的无标签镜像不自动清理。
- 删除前重新检查标签、归属及全部容器引用，只使用精确 `docker image rm`，不使用强制删除或全局 prune。Docker 自身也会保留被引用的父层。
- 同一工作区的构建和清理由 `.cache/docker-build.lock` 串行保护。构建失败不执行清理；清理失败打印错误，不掩盖已成功的构建结果。

清理不删除容器、数据卷、数据库或上传文件。回滚槽按构建顺序记录，重新使用创建时间较早的镜像也会得到保护；更早的镜像只要仍被容器引用，就继续保留。

只读预演（把 `local` 替换为本轮 `APP_IMAGE_TAG`）：

```bash
python3 deploy/image_cleanup.py cleanup --tag local --dry-run
```

回归验证不调用真实 Docker，不构建或删除真实镜像：

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s deploy/tests -p 'test_image_cleanup.py' -v
bash -n deploy/build.sh
```

历史专项构建脚本 `build-sharing*.sh` 不接入这条日常构建入口；BuildKit 的独立缓存也不由镜像清理脚本执行全局回收。
