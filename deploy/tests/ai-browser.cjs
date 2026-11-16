// AI_PLAYWRIGHT_MODULE=<runner>/node_modules/playwright node this-file OUTPUT
// The isolated Go fixture owns all accounts, objects and dependencies.
const fs = require('node:fs/promises');
const path = require('node:path');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const { chromium } = require(process.env.AI_PLAYWRIGHT_MODULE || 'playwright');

async function main() {
  const output = path.resolve(process.argv[2]);
  const ready = path.join(output, 'browser-fixture.json');
  const fixture = JSON.parse(await fs.readFile(ready));
  const browser = await chromium.launch({ headless: true, ...(process.env.AI_CHROMIUM_EXECUTABLE ? { executablePath: process.env.AI_CHROMIUM_EXECUTABLE } : {}) });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, acceptDownloads: true });
  await context.tracing.start({ screenshots: true, snapshots: true });
  const page = await context.newPage();
  page.setDefaultTimeout(30000);
  const result = { status: 'FAIL', checks: [], page_errors: [] };
  let token;
  page.on('pageerror', e => result.page_errors.push(e.message));
  page.on('response', async response => {
    if (new URL(response.url()).pathname === '/api/v1/auth/login' && response.ok()) {
      token = (await response.json()).access_token;
    }
  });
  const api = async (method, route, data, headers = {}) => {
    const res = await context.request.fetch(fixture.base_url + route, { method, data, headers: { Authorization: `Bearer ${token}`, ...headers } });
    assert(res.ok(), `API ${method} ${route}: ${res.status()}`);
    return res.status() === 204 ? null : res.json();
  };
  const eventually = async (predicate, label) => {
    const end = Date.now() + 90000;
    while (Date.now() < end) {
      if (await predicate()) return;
      await new Promise(resolve => setTimeout(resolve, 750));
    }
    throw new Error(`Timed out: ${label}`);
  };
  const upload = async (name, mimeType, buffer) => {
    await page.goto(fixture.base_url + '/drive');
    const done = page.waitForResponse(async res => {
      if (res.request().method() !== 'GET' || !/\/api\/v1\/uploads\/[a-f0-9-]+$/.test(new URL(res.url()).pathname)) return false;
      const data = await res.json().catch(() => ({}));
      return data.filename === name && data.status === 'completed';
    }, { timeout: 120000 });
    await page.locator('input[type=file]').setInputFiles({ name, mimeType, buffer });
    const session = await (await done).json();
    await page.goto(fixture.base_url + '/drive');
    result.checks.push('browser upload ' + name);
    return session.completed_entry_id;
  };
  const ask = async (ids) => {
    await page.getByRole('textbox', { name: '向文件提问' }).fill('rollback 回滚策略是什么');
    const response = page.waitForResponse(res => new URL(res.url()).pathname === '/api/v1/ai/ask');
    await page.getByRole('button', { name: '提问', exact: true }).click();
    const res = await response;
    assert.equal(res.status(), 200);
    const request = res.request().postDataJSON();
    assert.equal(request.scope.mode, ids.length ? 'selected' : 'all');
    if (ids.length) assert.deepEqual([...request.scope.file_ids].sort(), [...ids].sort());
    const answer = await res.json();
    assert(answer.citations.length > 0);
    for (const citation of answer.citations) {
      assert(citation.source_version_id && citation.content_version_id && citation.parent_id && citation.child_id);
      if (ids.length) assert(ids.includes(citation.file_id));
    }
    result.checks.push(ids.length ? `${ids.length} selected file Ask` : 'default all Ask');
    return answer;
  };
  const pick = async names => {
    await page.getByRole('button', { name: '选择文件', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: '选择问答文件' });
    await dialog.getByRole('button', { name: '清空草稿' }).click();
    for (const name of names) await dialog.getByRole('checkbox', { name: new RegExp(name.replace('.', '\\.')) }).check();
    await dialog.getByRole('button', { name: '应用选择' }).click();
    await dialog.waitFor({ state: 'hidden' });
  };
  try {
    await page.goto(fixture.base_url);
    await page.getByPlaceholder('用户名', { exact: true }).fill(fixture.username);
    await page.getByPlaceholder('密码', { exact: true }).fill(fixture.password);
    await page.getByRole('button', { name: /Enter cloud/ }).click();
    await page.waitForURL('**/drive');
    result.checks.push('login');
    const text = Buffer.from('browser rollback 文档回滚应保留历史数据，核查版本和快照。\n'.repeat(45));
    const textID = await upload('browser-ready.txt', 'text/plain', text);
    await eventually(async () => (await api('GET', `/api/v1/files/${textID}/ai`)).capabilities?.askable, 'text ready');
    result.checks.push('uploaded text becomes askable');
    await page.getByRole('row', { name: '查看 AI 摘要 browser-ready.txt', exact: true }).click();
    await page.getByRole('button', { name: '问此文件' }).click();
    await page.waitForURL('**/ask?*');
    await ask([textID]);
    await page.getByRole('button', { name: '清空选择' }).click();
    await ask([]);
    await pick(['alpha.txt']);
    const single = await ask([fixture.file_ids[0]]);
    await page.getByRole('button', { name: new RegExp(single.citations[0].file_name.replace('.', '\\.')) }).first().click();
    const citationDialog = page.getByRole('dialog');
    await citationDialog.getByText(single.citations[0].excerpt, { exact: true }).waitFor();
    const downloading = page.waitForEvent('download');
    await citationDialog.getByRole('button', { name: '下载原文件' }).click();
    const downloaded = await downloading;
    const file = path.join(output, 'browser-citation.txt');
    await downloaded.saveAs(file);
    assert((await fs.readFile(file, 'utf8')).includes('rollback'));
    await citationDialog.getByRole('button', { name: 'Close', exact: true }).click();
    result.checks.push('versioned citation excerpt and original download');
    await pick(['alpha.txt', 'beta.txt']);
    await ask(fixture.file_ids);
    const existing = await api('GET', `/api/v1/folders/${fixture.root}/children`);
    const beta = existing.items.find(file => file.id === fixture.file_ids[1]);
    assert(beta);
    await api('DELETE', `/api/v1/files/${beta.id}`, undefined, { 'If-Match': String(beta.version) });
    const question = '已删除文件的 rollback 处理';
    await page.getByRole('textbox', { name: '向文件提问' }).fill(question);
    const invalid = page.waitForResponse(res => new URL(res.url()).pathname === '/api/v1/ai/ask');
    await page.getByRole('button', { name: '提问', exact: true }).click();
    assert.equal((await invalid).status(), 409);
    await page.getByText(/部分文件已删除或尚未准备好/).waitFor();
    assert.equal(await page.getByRole('textbox', { name: '向文件提问' }).inputValue(), question);
    assert(await page.getByRole('button', { name: '提问', exact: true }).isDisabled());
    await page.screenshot({ path: path.join(output, 'browser-invalid-scope.png'), fullPage: true });
    result.checks.push('scope_invalid retains question and selection');
    const image = await page.screenshot({ clip: { x: 0, y: 0, width: 64, height: 64 } });
    const imageID = await upload('browser-image.png', 'image/png', image);
    await eventually(async () => (await api('GET', `/api/v1/files/${imageID}/preview`)).kind === 'image', 'thumbnail');
    await page.getByRole('row', { name: '预览图片 browser-image.png', exact: true }).click();
    const img = page.getByRole('dialog').getByRole('img', { name: 'browser-image.png' });
    await img.waitFor();
    await eventually(() => img.evaluate(node => node.complete && node.naturalWidth > 0), 'image preview rendered');
    await page.getByRole('dialog').getByRole('button', { name: 'Close', exact: true }).click();
    result.checks.push('image thumbnail and preview');
    const binary = crypto.randomBytes(1025);
    await upload('browser-unsupported.bin', 'application/octet-stream', binary);
    await page.getByRole('row', { name: '文件 browser-unsupported.bin', exact: true }).click();
    await page.getByText('此类型暂不支持在线预览，可下载查看。').waitFor();
    const binaryDownload = page.waitForEvent('download');
    await page.getByRole('dialog').getByRole('button', { name: '下载原文件' }).click();
    const binaryPath = path.join(output, 'browser-unsupported.bin');
    await (await binaryDownload).saveAs(binaryPath);
    assert.deepEqual(await fs.readFile(binaryPath), binary);
    result.checks.push('unsupported file information and verified download');
    assert.deepEqual(result.page_errors, []);
    result.status = 'PASS';
  } catch (e) {
    result.error = e.stack;
    process.exitCode = 1;
    await page.screenshot({ path: path.join(output, 'browser-failure.png'), fullPage: true }).catch(() => {});
  } finally {
    await context.tracing.stop({ path: path.join(output, 'browser-trace.zip') });
    await fs.writeFile(path.join(output, 'browser-results.json'), JSON.stringify(result, null, 2));
    await fs.writeFile(ready + '.done', result.status);
    await browser.close();
    console.log(JSON.stringify(result));
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
