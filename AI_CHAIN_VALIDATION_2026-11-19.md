# AI 精简后链路验收（2026-11-19）

本轮重新检查当前源码并执行测试，以 v3 直接执行为重点，回归默认 v2。不是沿用先前报告的通过结论。

**代码与隔离环境中的主要异步、查询、权限和恢复路径通过；当前配置不能直接认定为整套 AI 已正常运行。** 两个运行前提需要明确：实际 `deploy/.env` 没有显式启用协议 3，仍默认 v2；其中 `AI_VISION_MODEL=qwen-vl-plus` 实际调用返回 `404 model_not_found`。测试进程临时改用 `qwen3-vl-plus` 后，真实视觉、向量、摘要和问答链路通过。没有修改实际 `.env`、迁移存量库或部署服务。

## 实测结果

| 范围 | 本轮证据与结果 |
|---|---|
| 异步完整调用链 | 生产启动函数装配真实 HTTP、业务/控制/AI 三库、RabbitMQ、MinIO、Python parser、etcd、ES/Qdrant；source → text → vectors/summary → 独立投影 → overview/search/ask；v2/v3 均通过 |
| 收缩后的直接发布 | 真正执行 AI contract SQL，通用 artifact 表移入归档；视觉、文本、向量、摘要和查询均继续工作，摘要使用专用节点 |
| 原子发布与指针 | 成果、active 指针和稳定 receipt 在同一 AI 事务；在 receipt 写入前注入 SQL 异常，成果和指针一起回滚；恢复后可以提交 |
| 文本/向量替换 | 发布新 text 的同一事务清空并退休不兼容向量；此时全文和 ask 仍可使用文本，hybrid 可降级；新向量发布后恢复 semantic；旧输入的晚到向量被 superseded |
| 概览/摘要重建 | failed/pending 重建保留旧的可用发布成果；新摘要切换后概览读取新版本；summary-only 作业保留准确的发布引用；模型/流水线版本来自成果自身 |
| 视觉实际接口 | 当前配置 `qwen-vl-plus` 失败，明确为 provider `model_not_found/404`；临时使用 `qwen3-vl-plus` 后识别合成图片中的 BX7319，并经真实 embedding、summary、query embedding、answer 返回版本引用；没有把 mock 识别当成实际模型成功 |
| 扩容与缩容 | query 与五类 AI 后台角色从每种 1 个增加到 2 个；gRPC peer 地址证明两个真实 query 端点均经 etcd round_robin 到达；六个文件提交重建后撤下一组角色，剩余消费者完成作业，未产生重复 receipt、错配 active vectors 或通用 artifacts/control results |
| query 副本故障 | 仅切断第二个 query 副本代理的既有和新建 TCP 连接；连接收敛到健康副本后，真实 HTTP Search/Ask 成功；恢复连接后第二端点重新参与轮询，再验证缩容后的发现更新 |
| worker 崩溃 | 独立 OS 子进程在 receipt 前、AI COMMIT 后但控制 Complete 前、真实 AMQP ACK 前 SIGKILL；已提交效果通过稳定回执恢复；最后一次无可证明回执则 needs_attention，不盲目重算；后继不重复释放 |
| AI/业务库断连 | 仅切断夹具的 SQL 连接；AI PG 故障期间核心真实上传完成，恢复后原任务继续；业务 PG 故障期间已调度且输入齐备的摘要在 AI 本地发布，业务作业完成任务独立等待恢复 |
| ES/Qdrant 故障 | 单独/同时故障、恢复、投影覆盖证明；文本和向量分支独立回退 PG；无完整覆盖证明时不直接信任外部索引 |
| 权限与在途变化 | 跨用户 all/selected/folder、指定外部文件、overview/reprocess 越权检查；在召回屏障处并发 rename/move/delete，旧 Search/Ask 返回 409 scope_changed；新请求读取新元数据，删除文件不再召回；替换内容使用新的逻辑文件身份 |
| 收缩后的数据库权限 | 新建真实 query 登录账号，仅授予 published views；通过生产 OpenAI 的权限检查后运行 overview、fulltext/semantic/hybrid 和 ask；读取私有 summary nodes 返回 42501，UPDATE 返回只读事务错误 25006 |
| aigateway / gRPC 治理 | 服务 token、scope 签名、请求/trace 传递；全文只读重试、总 deadline/取消；Ask 与 semantic/hybrid 不透明重试模型；按方法/检索模式隔离熔断，调用参数/权限/配额错误不污染共享可用性；Redis 限流和阻塞等待有界 |

## 查询访问数据库的实际路径

`HTTP → aigateway → ai/v2 gRPC Query → core/v1 scope/分页授权 → AI published views → PG 或有覆盖证明的 ES/Qdrant → Core 再验 → 响应`。

API gateway 使用业务 SQL 读取身份、目录和已提交 epoch，签发短期 scope；文件名检索在 core 业务侧完成。Query 使用独立 AI PG 的只读账号，没有业务 SQL 池，也不连接控制 PG/MQ；Core 的 scope、文件引用、证据校验使用业务只读账号。ask 在模型/重排调用前及返回前复核证据。PG 向量查询会开启本地事务并设置 `SET LOCAL hnsw.*`，这是查询参数，不是写业务或 AI 成果。

异步发布跨库使用稳定 receipt 恢复：AI 本地成果与指针原子，控制侧完成及后继登记另一个本地事务；不宣称业务 PG、AI PG、控制 PG 三库之间存在全局事务。AI 重建允许旧的已发布版本在新版本完成前继续服务，source/text/vector/summary 是各自发布的能力，不承诺全部能力同时换成同一批次。

## 执行记录

- Go 1.25.13 linux/arm64。专用业务/控制/AI 库迁移分别为 37 / 6 / 12，均 dirty=false；夹具使用隔离 schema、MQ vhost、bucket、投影命名空间及故障代理。
- `TEST_AI_ONLINE=true TEST_REDIS_ADDR=127.0.0.1:6379 go test -json -p=1 ./... -count=1 -timeout=15m`：**42 个包通过、1059 个测试事件通过、12 个 skip、0 失败**；39 个包无测试文件。包含子测试/辅助入口，skip 不计为通过。其余 TEST_* 由专用环境提供。
- 真实模型单独执行 `TestDirectRealModelImagePipeline`。当前配置的失败日志与支持模型的成功日志均保留；全量中无真实模型配置时的 skip 不替代这项单独实测。
- 全量之后增强收缩后的真实 query 账号与副本 TCP 故障用例，分别执行专项 race，并再次执行相关包 vet。最终两包 race：**69 个测试事件通过、2 个 skip、0 失败**；另一次收缩后 query 权限专项 race 为 1 个通过。真实模型 v3 用例另为 1 个通过。
- `go vet -p=1 ./...`、相关包最终 vet、改动测试文件 gofumpt/goimports 和 `git diff --check`。

小样本响应测试使用 fake provider，每档 24 次混合 Search/Ask，非 race 全量中的扩容场景在缩容为一个副本后测得：

| 请求并发 | p50 | p95 |
|---|---:|---:|
| 1 | 46.09 ms | 55.76 ms |
| 4 | 51.52 ms | 65.43 ms |
| 8 | 70.17 ms | 96.11 ms |

这些值仅说明本机小夹具请求有响应，不是实际模型时延、扩容收益或容量结论。race 与真实模型的结果独立保留。

原始日志、逐项 skip、配置/运行快照及结果汇总位于 `artifacts/ai-chain-validation-20261119/`，不包含 provider 密钥或测试 env。初始失败及调试日志也保留：旧视觉模型的 404 是有效发现；online 矩阵的早期 ES/QD 断言失败来自新增文件未等待完整投影，本轮补齐了所有当前版本的就绪屏障；重叠启动独立测试夹具会因共享 etcd 服务前缀而发生服务 token 不匹配，因此最终 bootstrap 测试串行执行。

## 代码变更与运行边界

新增 `aisystem/direct_capabilities_test.go`、`bootstrap/ai_scale_integration_test.go`；增强真实模型用例以支持 v3，增强全链路夹具的副本生命周期与当前版本投影屏障。本轮没有修改生产执行逻辑；工作区中先前已有的控制平面修复及故障测试完整保留。

当前可见 Docker 容器为专用测试基础设施，没有把当前运行环境描述为已经部署 v3。实际 AI public schema 仍存在通用 artifact 表；contract 的通过证据来自隔离夹具。后续启用 v3 仍需按交接/激活流程处理存量证据、预算和旧写者，不能只改协议开关。

建议实际配置将 `AI_VISION_MODEL` 从 `qwen-vl-plus` 改为本轮已验证的 `qwen3-vl-plus`。此项没有写入实际 `.env`，也没有重启任何业务角色。

三库共用一个 PG17 实例，专用实例的 fsync/full_page_writes 均为 off；SIGKILL 测的是应用进程，不能作为数据库掉电持久性证明。副本运行在同一宿主机的生产启动函数中，独立 SQL 池/队列消费者和真实服务发现已验证，不等同于多机或 Docker 部署的完整 HA。TCP 副本故障实测允许连接收敛，不保证断连瞬间所有在途请求都成功。

没有执行多机网络分区、PG 故障转移/PITR、多节点 MQ/对象存储/etcd、持续容量压测；普通池/prefetch/并发仍按消费者增加，不是跨副本总预算。模型检查点落库前仍可能有外部调用未知成功窗口；稳定任务回执保证领域幂等，不能保证外部模型恰好调用一次。实际模型测试使用合成图片和短文本，不代表所有格式或模型质量已验收。
