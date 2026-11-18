# 在线 AI 网关、检索与问答验证

本次从实际 HTTP API 的 `aigateway` 进入，经过带服务身份校验的 AI v2 gRPC、AI Query、core RPC、发布视图和 ES/Qdrant/PG 召回，执行故障、恢复、并发及权限变化验证。发现并修复两个问题：协调 Redis 无响应会拖慢在线请求；范围内无可用向量会误计入共享语义熔断器。

## 环境与证据边界

- 本次专属目录：`artifacts/ai-online-check-20261119T050316/`。目录名是本次运行标识，日志中的 UTC 时间为执行时间。
- 七个临时容器：PG、RabbitMQ、MinIO、Elasticsearch、Qdrant、etcd、Redis。使用独立名称、随机本机端口和 tmpfs；没有连接已有业务数据。
- 业务、控制、AI 使用三个专用数据库，位于同一 PG 实例。测试各自建独立 schema，query/compute/commit 使用不同 AI 角色，业务 worker 使用只读连接。
- API、core-internal、AI Query、各计算及提交角色调用实际 bootstrap 启动函数，在同一 Go 测试进程中运行；parser 使用真实 Python 子进程。实际 HTTP/gRPC/MQ/对象存储请求穿过真实基础设施，服务发现使用真实 etcd。
- 模型使用确定性 fake provider。传输错误、超时和限流由可控服务/代理注入。本报告不证明真实模型供应商的质量、配额、延迟或故障切换，也不代表多机高可用或容量认证。
- 网关治理测试通过真实 HTTP、实际 bootstrap gRPC 拦截器及服务身份校验连接受控 AIQuery 服务；完整检索、发布和权限链由 `TestAISplitRealDependencies` 单独覆盖。
- 没有部署本次代码，没有启动或修改原有应用容器，没有修改 `deploy/.env`。

## 已修复的问题

### 1. Redis 限流等待未受在线预算约束

故障端点接受 TCP、读取客户端数据，但不返回任何 Redis 响应。限流发生在查询预算之前，原协调客户端又未启用 context 超时，导致请求等待约 **5.057 秒**后才按原有 fail-open 策略进入 AIQuery。

修复：`internal/ai/rate_limit.go` 给限流脚本设置 100 ms 总预算；`internal/platform/dependencies/dependencies.go` 的协调客户端启用 `ContextTimeoutEnabled`，使预算约束连接池和网络等待。

修复后同一场景耗时约 **103 ms**，HTTP 200、下游只调用一次。同一客户端恢复到真实 Redis 后，第一次请求通过，下一次按每分钟 1 次的配置返回 429，证明恢复后重新执行限流。

证据：`redis-wait-before.jsonl`、`redis-wait-after.jsonl`、`online-acceptance.jsonl`。

Redis 故障时仍沿用现有 fail-open 策略，所以 Redis 不可用期间不保证硬限流。本次只约束等待时间并验证恢复行为。

### 2. 范围内向量未就绪会影响其他用户

无文件/无可用向量的用户进行 semantic Search，会收到已有契约中的 503 `ai.semantic_unavailable`。原 gRPC 客户端把这类 `Unavailable` 一律计入共享依赖熔断，连续请求可使正常用户的语义请求被直接拒绝。

修复：AIQuery 将“当前范围没有向量或没有兼容向量”标为请求范围未就绪，通过内部 `ErrorInfo` 携带分类。`grpcx` 对该分类不计依赖失败、不重试；对外 HTTP/gRPC 错误契约保持一致。模型/PG/传输故障仍按依赖故障熔断。

修复后，空用户连续 25 次请求均返回原来的语义未就绪错误，随后正常用户仍通过 Qdrant 得到结果。普通 `Unavailable` 的打开、半开和恢复测试仍通过。范围失效、范围变化、供应商配额错误也不会误开共享熔断器。

证据：`readiness-before.jsonl` 的复现及 `online-acceptance.jsonl` 的修复后验证。

## 已执行的在线场景

| 场景 | 实际断言 |
| --- | --- |
| fulltext 读重试 | 首次 Unavailable 后成功，下游共 2 次；持续失败最多 3 次，返回 503 |
| semantic / hybrid / Ask | 下游 Unavailable 时各调用 1 次，返回 503，不重复执行模型请求 |
| 总超时与取消 | 查询总预算耗尽返回 504 `ai.query_timeout`；退避共享总预算；HTTP 取消传到 Ask，不重放 |
| 熔断隔离与恢复 | Ask、fulltext、semantic、hybrid 分开统计；打开后不访问下游；超时后半开探测成功，原客户端恢复调用 |
| 并发限流 | 同用户同时 12 次 Search + 12 次 Ask，限额分别为 3/2：5 次 200、19 次 429，仅 5 次到达 AIQuery |
| Redis 故障与恢复 | 拒绝连接和无响应均按 fail-open；无响应等待有界；同一协调客户端恢复限流 |
| ES/QD 故障与恢复 | healthy、ES down、QD down、both down、recovered；fulltext/semantic/hybrid 及单文件、多文件、全部范围 Ask 都保留可用证据；故障分支回退 PG；恢复后重新使用 ES/QD，降级标志清除 |
| QD 超时 | QD 读取被阻塞至投影客户端超时，semantic Search 使用 PG 向量返回结果；解除故障后原链路重新使用 QD |
| 认证及越权 | 无/无效 bearer 为 401；另一用户的名称/全文/hybrid 搜索为空，Ask 无引用；指定他人文件/文件夹为 409；他人 AI 状态、重建、重命名、删除为 404 |
| 空范围 semantic | 503 `ai.semantic_unavailable`，没有他人结果；重复请求不影响正常用户 |
| 请求中途改名/移动/删除 | 每种变更同时挂起 3 个 semantic Search 和 3 个选定文件 Ask；实际 HTTP 变更提交后释放召回，6 个旧请求全部返回 409 `ai.scope_changed` |
| 变更后的新请求 | Ask 引用新文件名；文件移出根目录直属范围后不再召回；删除后不再搜索到，旧 selected 范围为 409 |
| 内容替换 | 当前没有原地修改字节的接口，使用删除后同名上传；新内容可搜、Ask 引用新 ID，旧 ID 始终失效 |
| RPC 可观测与身份 | 实际拦截器验证 request ID/trace 传播、客户端/服务端 span 和指标、错误服务 token 拒绝、panic 处理 |

修改期间的全链路屏障位于“core 已解析范围、外部召回尚未返回”的阶段。`aisystem` 回归另执行 `TestEvidenceRevalidationAtModelBoundaries`，验证 reranker 前、Answer 前及 Answer 中途权限变化会阻止输出；这些模型阶段验证使用受控 core/provider，不能写成真实 HTTP 修改发生于供应商生成期间。

## 有限并发观察

每档执行 24 个混合请求：fulltext、semantic、hybrid、选定两文件 Ask。各请求都断言状态及证据，Ask 引用必须位于指定范围。

| 并发数 | 请求数 | p50 | p95 |
| --- | --- | --- | --- |
| 1 | 24 | 46.8 ms | 54.2 ms |
| 4 | 24 | 49.9 ms | 81.8 ms |
| 8 | 24 | 56.9 ms | 78.4 ms |

以上来自 `online-acceptance.jsonl`，仅是本机 fake 模型、小语料、短时有限并发的观察值。测试期间已关闭完整链路的限流，限流由独立治理测试开启验证。

## 检查结果与复跑

`online-acceptance`：39 个测试/子测试记录通过，0 失败、0 跳过；该数字包含父测试记录。覆盖新增治理测试、实际完整链路及原 RPC 治理测试。

`regression`：`ai`、`aigateway`、`aisystem`、`core`、`platform/grpcx`、`platform/breaker`、`platform/dependencies` 七个相关包通过，299 个测试/子测试记录通过、0 失败、2 跳过。跳过项为未启用 `TEST_CHINESE_PARSER_WORKER=1` 的旧 `TestChineseParserToSearchPipeline`，以及需要真实供应商配置的 `TestRealModelImagePipeline`；本次实际 parser 链由完整链路用例执行。

本轮同时重新执行既有 HNSW 回归：5001 个向量的 `EXPLAIN (ANALYZE, BUFFERS)` 确认 query 角色通过发布视图使用 `ai_vectors_cosine`，未强制关闭顺序扫描，样本执行时间 0.913 ms；过滤后短召回的精确补足、范围及发布状态隔离、会话设置回滚测试均通过。这证明前序 PG 向量回退修复在当前代码下仍成立，该计时不代表线上规模下的性能。

`race-online`：与在线验收相同的 39 个测试/子测试记录在 `-race` 下全部通过，0 失败、0 跳过，未报告数据竞争。

`go vet -p=1 ./...`、`go build -p=1 ./cmd/...`：均通过。使用 Go 1.25.13、`GOMAXPROCS=2`，按串行包预算执行。`git diff --check` 及修改 Go 文件的 gofmt 检查通过。

新测试文件：`backend/internal/bootstrap/ai_gateway_governance_test.go`、`ai_online_integration_test.go`。原 split-chain 用例仅新增可选故障屏障及 `TEST_AI_ONLINE=true` 的在线检查入口，仍需 `TEST_AI_SPLIT=true` 和专用测试基础设施。

复跑完整在线检查，在 `backend/` 下设置专用测试连接并执行：

```bash
TEST_AI_SPLIT=true TEST_AI_ONLINE=true PYTHONDONTWRITEBYTECODE=1 \
go test -json -p=1 ./internal/bootstrap \
  -run '^(TestAISplitRealDependencies|TestAIGateway|TestInternalRPCTraceGovernance)' \
  -count=1 -timeout=8m
```

所需连接变量见 `AGENTS.md` 和 fixture 开头。业务专用测试库的 `public` 也必须完成迁移，并给业务只读角色 SELECT；启动检查会读 `public.file_entries`，只初始化 fixture schema 不够。当前测试初始化已补齐，起初缺少 public 导致的夹具失败没有计入通过结果。其他初始断言已按实际契约修正：错误码位于 `error.code`，重建请求必须携带 `Idempotency-Key`，空范围 semantic 返回 503。

本次专属 `infra.py`、`run.py`、各阶段 JSONL/summary、源码哈希清单保存在上述 artifacts 目录，均未入库。私有连接文件只用于临时资源，不应复制到报告或提交。

七个本次专属临时容器已停止并自动删除，其 tmpfs 数据随容器回收；对比回收前后清单，其他 130 个已有容器的名称和状态保持一致。证据保留于 `cleanup-summary.json`，未删除运行日志。首次 parser 运行产生的已跟踪 pyc 字节变化已恢复，保留本次源码和测试改动。
