"""Evaluate retained raw evidence against the deployed HTTP contract, then report."""
import collections
import datetime
import json
from pathlib import Path
import sys

out = Path(sys.argv[1])
def load(name):
    return json.loads((out / name).read_text())
initial, supplement, edges = load('first-attempt-results.json'), load('supplement-results.json'), load('edges-results.json')
gc, browser, soak = load('edges-gc-corrected-results.json'), load('browser-results.json'), load('soak-results.json')
reconciliation, db = load('reconciliation-results.json'), load('db-post-soak.json')
storage, indexes, broker = load('storage-audit.json'), load('index-audit.json'), load('broker-audit.json')
intent_audit = load('intent-audit.json')
stability = load('stability-audit.json')
assert stability['status'] == 'PASS'
expected_initial = {'AI-reprocess-job-terminal-and-job-owner-isolation', 'spike-32-users-320-requests-and-8-parallel-real-uploads'}
assert {g['name'] for g in initial['groups'] if g['status'] == 'FAIL'} == expected_initial
dedup_case = '4-parallel-same-content-uploads-exactly-one-owner-entry'
assert {g['name'] for g in edges['groups'] if g['status'] == 'FAIL'} == {'last-reference-delete-respects-configured-GC-delay',dedup_case}
checks = [g for g in initial['groups'] if g['status'] == 'PASS'] + supplement['groups'] + [g for g in edges['groups'] if g['name'] != 'last-reference-delete-respects-configured-GC-delay'] + gc['groups'] + reconciliation['groups']
assert all(g['status'] == 'PASS' or g['name'] == dedup_case for g in checks)
assert all(r['status'] == 'PASS' for r in [browser, storage, indexes, broker])
assert soak['soak']['elapsed_ms'] >= 600000
assert len(soak['soak']['counts']) == 6 and min(soak['soak']['counts']) >= 1000
assert soak['status'] == 'PASS' and not soak['soak']['unexpected'], 'unclassified soak failure'
assert soak['soak']['session_rotations'] == 2, 'session rotation was not exercised'
http_counts = collections.Counter()
for request in soak['requests']:
    status, code = request.get('status'), request.get('code', '')
    http_counts[str(status) + (':' + code if code else '')] += 1
    assert status in [200, 201, 202, 204] or (status == 409 and code == 'ai.scope_changed') or (status == 503 and code == 'service.overloaded'), request
assert not db['receipts']['missing_or_conflicting']
assert not db['missing_inbox'] and all(int(t['actionable']) == 0 for t in db['tasks'])
assert all(int(r['violations']) == 0 for r in db['business_checks'] + db['ai_checks'])
assert not db['task_errors'], 'current run contains stopped/actionable task'
historical = storage['historical_reference_mismatches']
assert len(historical) == 2 and all(r['bucket'] == 'test' and r['created_at'].startswith('2026-09-04') for r in historical)
intents = intent_audit['pending_terminal_intents']
assert len(intents) == 3 and intent_audit['status'] == 'FAIL'
assert all(i['state'] == 'pending' and i['upload_status'] == 'failed' and i['reserved_bytes'] == 0 and all(t['state'] == 'done' and t['outcome'] == 'failed' and t.get('private_output_exists') is False for t in i['tasks']) for i in intents)
spike = next(g['evidence'] for g in supplement['groups'] if g['name'].startswith('spike-32'))
parallel = next(g['evidence'] for g in supplement['groups'] if g['name'].startswith('8-concurrent'))
summary = dict(status='FAIL_INTENT_CONVERGENCE', evaluated_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
    functional_groups=len(checks), functional_pass=sum(g['status']=='PASS' for g in checks), functional_fail=sum(g['status']=='FAIL' for g in checks), browser_checks=browser['checks'], soak=soak['soak'], soak_http_counts=dict(http_counts),
    spike=spike, parallel_upload=parallel, receipts=db['receipts'], events=db['events'], quota=db['quota'],
    storage_objects=len(storage['objects']), storage_bytes=sum(o['bytes'] for o in storage['objects']),
    future_tasks=db['future_tasks'], historical_reference_mismatches=historical,pending_terminal_intents=intents,
    raw_soak_status=soak['status'], stability=stability,contract_recognized_scope_conflicts=sum(r.get('status') == 409 for r in soak['requests']))
(out / 'acceptance-summary.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2) + '\n')
tz = datetime.timezone(datetime.timedelta(hours=8))
def local(timestamp):
    return datetime.datetime.fromisoformat(timestamp.replace('Z', '+00:00')).astimezone(tz).strftime('%Y-%m-%d %H:%M:%S')
quota_rows = '\n'.join(f"| {q['username'].rsplit('_',1)[-1]} | {q['actual_logical_bytes']} | {q['occupied_bytes']} | {q['session_reserved_bytes']} |" for q in db['quota'])
future_rows = '\n'.join(f"| `{t['kind']}` | {t['count']} | {local(t['earliest'])} |" for t in db['future_tasks'])
historical_rows = '\n'.join(f"| `{r['id']}` | {r['bucket']} | {r['reference_count']} | {r['actual']} | {r['created_at']} |" for r in historical)
intent_rows = '\n'.join(f"| `{r['id']}` | `{r['upload_session_id']}` | {r['state']} | {r['upload_status']} | done / failed |" for r in intents)
text = f'''# 已部署链路最终黑盒验收（2026-11-20）

结论：**最终完整数据验收未完全通过**。主要用户链路、完整 10 分钟持续负载、业务引用/配额/对象字节、控制结果与回执均通过；但本轮并发同内容上传留下 **3 条已经失败终结、旧操作意图仍为 pending 的 canonical.ensure 记录**。此外全库有 2 条验收前已存在的历史引用异常。未修改或清理这些数据，也未修改业务实现。

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
| 浏览器 | 真实登录、建目录、选文件、storage PUT、刷新、浏览器下载哈希、第二标签页、390px 视口共 {len(browser['checks'])} 项通过；无 pageerror |
| 最后引用删除 | 引用为 0，访问 404；object.authorize_delete 按零引用时刻 + 24h 调度，宽限内保留字节符合配置 |

真实模型的网络、余额失败原本允许单列，但本轮所测查询、问答、摘要和投影样本实际成功。未知二进制格式不要求产生 AI 内容；这类任务按领域结论收尾。

## 尖刺与完整浸泡

- 尖刺：32 个并发用户循环，共 320 次请求，{spike['accepted']} 次 200、{spike['controlled_503']} 次 `503 service.overloaded`；拒绝携带 `Retry-After: 1`。P95 {spike['p95_ms']:.2f} ms / P99 {spike['p99_ms']:.2f} ms。等待 1.1 秒后连续 10 次目录请求全部 200，合计 {spike['recovery_10_success_ms']} ms。
- 8 个并发真实上传均 completed，逐个下载 SHA-256 一致；最长端到端 {parallel['max_e2e_ms']} ms。
- 有效浸泡：北京时间 **{local(soak['soak']['started_at'])} 至 {local(soak['soak']['finished_at'])}**，实际 {soak['soak']['elapsed_ms']/1000:.3f} 秒。6 个读循环全程保活，每循环次数 {soak['soak']['counts']}，另有目录写入 {soak['soak']['writes']} 次与 {soak['soak']['uploads']} 次真实上传/下载；会话按正常客户端流程完成 {soak['soak']['session_rotations']} 次 refresh cookie 轮换。
- 共 {soak['soak']['requests']} 次记录请求，{soak['soak']['rps']:.2f} 请求/秒，P95 {soak['soak']['p95_ms']:.2f} ms、P99 {soak['soak']['p99_ms']:.2f} ms。状态分布：`{json.dumps(dict(http_counts),ensure_ascii=False)}`。
- 并发修改期间的 `409 ai.scope_changed` 表示旧授权快照失效；`503 service.overloaded` 表示有界准入拒绝，均单独计数。没有未分类 HTTP 错误、网络超时或崩溃。各容器未重启、未 OOM，最终健康状态全部 healthy。

## 最终数据核对

结束持续写入后，核对全部 3 个测试账号拥有过的上传、逻辑文件、对象及其任务，包含浏览器和补验数据，而非仅核对脚本内存里的成功文件。

- 本轮事件 {db['events']['total']} 条全部进入控制 inbox；待处理到期任务和 running 任务为 0，stopped 为 0。
- {db['receipts']['core_results']} 条核心结果、{db['receipts']['ai_results']} 条 AI 结果与领域回执逐项比对，缺失/冲突为 0。
- 引用计数、completed 会话的 entry/object/owner、有效 entry 的对象 ready 状态、终态预留均一致；配额以 `user_quota_usage.occupied_bytes` 为权威，旧 users 计数字段不作为验收依据。
- AI source/text/summary/vector 活跃指针、父子片数量、向量数量、projection complete 的 expected/applied 数量全部一致。
- MinIO {len(storage['objects'])} 个对象、{sum(o['bytes'] for o in storage['objects'])} 字节逐个核验大小和 SHA-256；本轮 staging=0、未完成 multipart=0。
- 真实 ES {sum(x['children'] for x in indexes['es'])} 个活跃子片与 PG 内容/父片/版本一致；Qdrant {sum(x['vectors'] for x in indexes['qdrant'])} 个活跃 1024 维向量与 PG 一致，cosine ≥ 0.999999。
- 当前协议 {len(broker['queues'])} 个队列 ready=0 / unacked=0，work 消费者均存在。
- 间隔 {stability['seconds_between_snapshots']:.3f} 秒二次核对，业务/控制/AI 的 13 组数据无变化；这也确认下述 3 条旧意图没有自然终结。

| 用户 | 有效逻辑字节 | occupied_bytes | 会话预留 |
|---|---:|---:|---:|
{quota_rows}

未到期任务仍保留，不能以数据库任务总数非零判断未收敛：

| 类型 | 数量 | 最早执行时间（北京时间） |
|---|---:|---|
{future_rows}

## 本轮未收敛的旧操作意图

以下记录 available_at 已过、上传会话已经 failed、控制任务已经 done / failed，原始计算结果和领域回执均存在；但业务库 storage_operations 仍为 pending。3 个失败计算输出均已直接向 MinIO HEAD 确认不存在，因此本轮未发现对应字节泄漏，配额预留为 0。控制库是唯一执行权威，旧意图不会自行重跑；但旧意图的状态没有表达终结结论，不能将其写成全表数据已收敛。

| 意图 ID | 上传会话 | 意图状态 | 会话状态 | 控制状态 / 结论 |
|---|---|---|---|---|
{intent_rows}

复现：同一 owner 并发创建 4 个不同文件名、相同 SHA-256/大小的上传，在成功建 entry 之前全部完成分片并提交。1 个会话成功，3 个在提交时发现 owner 已有同对象 entry，失败收尾。当前源码 `backend/internal/asyncjobs/committer.go` 的 ErrNameConflict 分支改走 out.Convergence；`backend/internal/upload/async_commit.go` 的收尾分支在 markIntent 之前返回，留下原 canonical.ensure 意图的 pending 状态。这个问题本轮已定位和留证，**尚未修复或重新部署**。

原始 dedup 用例最先因沿用旧 upload.file_exists 预期而失败；后续检查源码确认当前公开终态码为 upload.commit_precondition_failed，并进一步只读核对发现上述 3 条实质状态残留。没有仅通过放宽断言将该场景改判成功。独立 `intent-audit.json` / `final-intent-audit.py` 明确返回 FAIL。

## 历史遗留与验收脚本校正

以下 2 条异常在开始前已存在，结束时数量和身份未改变；不属于本轮数据，未修改：

| 对象 | bucket | reference_count | 实际活跃引用 | 创建时间（UTC） |
|---|---|---:|---:|---|
{historical_rows}

首轮验收脚本有几处预期错误，原始失败文件均保留：重建漏幂等键导致 400、尖刺将准入 503 误判成错误、混合写入时未接纳 ai.scope_changed，以及误把 gc_eligible_at 当作最终删除时间。重建、尖刺、GC 均用修正后的请求/断言重新实测通过。首轮短浸泡因读循环退出而作废，明确停止本轮自有压测进程。

另一次整段浸泡虽达到 600 秒，但复用了已签发数分钟的 access token，末段达到 900 秒 TTL 后出现 697 次 `401 auth.unauthorized`；该次明确判为 FAIL，保留于 `soak-token-expiry-results.json`，没有计入有效验收。最终重新真实登录并增加主动会话轮换，从零完整执行上述 600 秒，原始 runner 状态为 **{soak['status']}**，unexpected=0。所有原始 JSON 均保留。浏览器早期定位器及上传跳转页的脚本错误亦保留，最终浏览器验证为 PASS。

## 证据与复跑

原始材料：`{out}`，汇总 `acceptance-summary.json`。包括 `first-attempt-results.json`、`supplement-results.json`、`edges-results.json`、`edges-gc-corrected-results.json`、`browser-results.json`、`soak-results.json`、`reconciliation-results.json`、`db-baseline.json` / `db-post-soak.json`、`storage-audit.json`、`index-audit.json`、`broker-audit.json`、`intent-audit.json`、资源样本与浏览器截图。私有 fixture 包含测试登录凭据，目录权限 0700，勿公开发布。

复跑使用新增 `deploy/tests/final-*.mjs` / `final-*-audit.py`；主入口 `FINAL_OUTPUT=artifacts/<新目录> node deploy/tests/final-acceptance.mjs`，浏览器入口 `node deploy/tests/final-browser.mjs artifacts/<目录>`。后者可用 FINAL_PLAYWRIGHT_MODULE / FINAL_CHROMIUM_EXECUTABLE 指定本机浏览器运行器。库、bucket、卷及测试数据不会自动清理。相关脚本已完成 Node 语法、Python AST 和 git diff --check 检查；未改业务代码，因此未重跑后端全量单元测试。
'''
Path('FINAL_BLACKBOX_ACCEPTANCE_2026-11-20.md').write_text(text)
print(json.dumps({k: summary[k] for k in ['status','functional_groups','storage_objects','storage_bytes','receipts']},ensure_ascii=False))
