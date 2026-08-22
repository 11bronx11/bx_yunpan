# 产品核心性能评测

日期：2026-08-22 UTC  
测试目标：验证 AI 改动开启后，网盘核心只读路径的吞吐和延迟没有明显退化。该测试不做业务缺陷枚举，也不改变文件内容。

## 核心读路径

`product-core-performance.js` 每次迭代读取：目录根、目录 children、用户配额、活动上传会话、分享列表。负载为 40 iterations/s，持续 15s。

- 601 iterations；每次最多 5 个接口，总计 3005 HTTP requests；约 200.25 requests/s；
- HTTP 错误率 0%；检查通过率 100%；
- HTTP 请求平均 1.16ms，P95 2.92ms，最大 20.83ms；
- 自定义 endpoint 延迟平均 1.26ms，P95 3ms。

`product-http-performance.js` 另测 `/health/live` 和目录根/children，持续 10s、总计 2202 requests，HTTP 错误率 0%，请求 P95 2.85ms。

## Multipart 上传

3 个账号并发执行 8 MiB 单分片上传（创建、预签名、MinIO PUT、确认、完成和 Worker 校验），3/3 成功；端到端耗时平均 4242ms，最小 4224ms，最大 4255ms。使用 `application/octet-stream`，因此没有把模型索引耗时混入上传数字。

## 与旧基线的关系

根目录 `benchmark.md` 的 3472.86 req/s 是 20 VU、20s、只请求 `GET /folders/:id/children` 的专用基线；本轮是多接口混合读取（每次迭代 5 个接口），因此不直接宣称比旧值快或慢。旧 benchmark 中的 multipart 和 backlog 数据也作为历史参考保留，没有覆盖。

## 运行环境

环境快照在 `environment.txt`：4 CPU cores、5.2 GiB 内存、约 8.0 GiB 磁盘可用；服务包含 PostgreSQL、Redis、MinIO、Worker、API 和 3 个 aisvc 副本。

真实运行输出：`product-core-k6.txt`、`product-core-summary.json`、`product-http-k6-rerun.txt`。

## 边界

这是一轮短时单机性能烟测，不是容量上限、长时间 soak、故障恢复或多机横向扩展结论。上传、下载和 MinIO 直传需要单独按文件大小、并发分片数和对象存储带宽建立容量基线；本轮没有把它们混入核心读路径数字。
