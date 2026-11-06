import assert from 'node:assert/strict';
import {createHash, randomUUID} from 'node:crypto';
import {existsSync, mkdirSync, readFileSync, writeFileSync} from 'node:fs';
const {chromium} = await import(process.env.FULLCHAIN_PLAYWRIGHT_MODULE ?? '/home/bronx/.npm-global/lib/node_modules/@playwright/mcp/node_modules/playwright/index.mjs');
const base = process.env.FULLCHAIN_BASE ?? 'http://127.0.0.1:24000';
const output = process.env.FULLCHAIN_OUTPUT ?? 'artifacts/fullchain-20261106';
assert.equal(new URL(base).hostname, '127.0.0.1');
mkdirSync(output, {recursive: true});
const report = {base, started_at: new Date().toISOString(), checks: [], page_errors: [], api_errors: []};
const installedBrowser = '/home/bronx/.cache/ms-playwright/chromium-1228/chrome-linux/chrome';
const browser = await chromium.launch({headless: true, ...(existsSync(installedBrowser) ? {executablePath: installedBrowser} : {})});
const contexts = [];
const label = text => new RegExp(text.split('').join('\\s*'));
const save = () => writeFileSync(`${output}/browser-results.json`, JSON.stringify(report, null, 2) + '\n');
async function device() {
  const context = await browser.newContext({viewport: {width: 960, height: 900}, reducedMotion: 'reduce', acceptDownloads: true});
  contexts.push(context);
  const page = await context.newPage(); page.setDefaultTimeout(30000);
  page.on('pageerror', e => report.page_errors.push(e.message));
  page.on('response', r => {if (r.status() >= 500 && r.url().includes('/api/')) report.api_errors.push({path: new URL(r.url()).pathname, status: r.status()});});
  return page;
}
async function register(page) {
  const account = {username: `browser-${randomUUID().slice(0, 10)}`, password: `Browser-${randomUUID()}!`};
  await page.goto(`${base}/login`);
  await page.getByRole('button', {name: 'Create account', exact: true}).click();
  await page.getByPlaceholder('name@example.com').fill(`${account.username}@example.test`);
  await page.getByPlaceholder('用户名', {exact: true}).fill(account.username);
  await page.getByPlaceholder('密码', {exact: true}).fill(account.password);
  await page.getByPlaceholder('再次输入密码').fill(account.password);
  await page.locator('button[type=submit]').click();
  await page.waitForURL('**/drive');
  await page.getByRole('button', {name: label('新建目录')}).waitFor();
  return account;
}
async function login(page, account) {
  await page.goto(`${base}/login`);
  await page.getByPlaceholder('用户名', {exact: true}).fill(account.username);
  await page.getByPlaceholder('密码', {exact: true}).fill(account.password);
  await page.locator('button[type=submit]').click();
  await page.waitForURL('**/drive');
  await page.getByRole('button', {name: label('新建目录')}).waitFor();
}
function pass(name, evidence) {report.checks.push({name, pass: true, evidence}); console.log(`PASS ${name}`); save();}
try {
  const a = await device(), b = await device(), c = await device();
  const owner = await register(a); await login(b, owner);
  const recipient = await register(c); void recipient;
  pass('UI registration and simultaneous login in two independent browser contexts');
  const folderName = `browser-race-${randomUUID().slice(0, 8)}`;
  await Promise.all([a, b].map(async p => {
    await p.getByRole('button', {name: label('新建目录')}).click();
    await p.getByRole('textbox', {name: '目录名称', exact: true}).fill(folderName);
  }));
  const responses = await Promise.all([a, b].map(async p => {
    const response = p.waitForResponse(r => new URL(r.url()).pathname === '/api/v1/folders' && r.request().method() === 'POST');
    await p.getByRole('dialog').getByRole('button', {name: label('创建')}).click();
    return (await response).status();
  }));
  assert.deepEqual(responses.sort(), [201, 409]);
  await Promise.all([a.reload(), b.reload()]);
  await a.getByText(folderName, {exact: true}).waitFor(); await b.getByText(folderName, {exact: true}).waitFor();
  pass('two devices create the same directory: one 201 and one controlled 409', responses);
  const filename = `browser-proof-${randomUUID().slice(0, 8)}.txt`;
  const bytes = Buffer.from('浏览器验收文档：海岚项目的恢复口令是 BROWSER915。PostgreSQL 管理元数据，MinIO 保存原始文件，Elasticsearch 与 Qdrant 支持搜索。');
  await a.locator('input[type=file]').setInputFiles({name: filename, mimeType: 'text/plain', buffer: bytes});
  await a.waitForURL('**/transfers');
  await a.getByText('已完成', {exact: true}).first().waitFor({timeout: 150000});
  await a.goto(`${base}/drive`);
  await a.getByRole('button', {name: `管理 ${filename}`, exact: true}).waitFor({timeout: 150000});
  await b.reload(); await b.getByRole('button', {name: `管理 ${filename}`, exact: true}).waitFor();
  pass('UI upload crosses multipart/worker verification and is visible on the other device');
  await b.getByRole('button', {name: `管理 ${filename}`, exact: true}).click();
  const downloadEvent = b.waitForEvent('download');
  await b.getByRole('menuitem', {name: label('下载')}).click();
  const download = await downloadEvent; const filePath = `${output}/browser-download.txt`; await download.saveAs(filePath);
  const hash = value => createHash('sha256').update(value).digest('hex');
  assert.equal(hash(readFileSync(filePath)), hash(bytes));
  pass('browser downloads actual bytes with matching SHA256');
  await a.getByRole('button', {name: `管理 ${filename}`, exact: true}).click();
  await a.getByRole('menuitem', {name: label('分享')}).click();
  const shareDialog = a.getByRole('dialog'); await shareDialog.waitFor();
  await shareDialog.getByRole('button', {name: label('创建分享'), exact: true}).click();
  const shareKeyInput = shareDialog.getByRole('textbox', {name: '分享 Key', exact: true});
  await shareKeyInput.waitFor();
  const shareKey = await shareKeyInput.inputValue(); assert.equal(shareKey.length, 22);
  await c.getByRole('button', {name: label('导入分享')}).click();
  await c.getByPlaceholder('粘贴分享 Key').fill(shareKey);
  await c.getByRole('dialog').getByRole('button', {name: label('解析分享')}).click();
  await c.getByRole('dialog').getByRole('button', {name: label('导入到当前目录')}).click();
  await c.getByRole('button', {name: `管理 ${filename}`, exact: true}).waitFor();
  await shareDialog.getByRole('button', {name: label('关闭')}).click();
  pass('second account imports a real share through the UI');
  await a.goto(`${base}/shares`);
  const shareRow = a.getByRole('row').filter({hasText: filename});
  await shareRow.getByText('有效', {exact: true}).waitFor();
  await shareRow.getByRole('button', {name: label('修改有效期')}).click();
  await a.getByRole('dialog').locator('.ant-select-selector').click();
  await a.getByText('长期有效', {exact: true}).last().click();
  await a.getByRole('dialog').getByRole('button', {name: label('保存')}).click();
  await shareRow.getByText('长期有效', {exact: true}).waitFor();
  pass('share management displays the file and updates expiry through the versioned API');
  await a.setViewportSize({width: 390, height: 844});
  assert.ok((await a.locator('body').evaluate(e => e.scrollWidth)) <= 390, 'mobile layout overflow');
  await a.screenshot({path: `${output}/sharing-mobile.png`, fullPage: true});
  await a.setViewportSize({width: 960, height: 900});
  await shareRow.getByRole('button', {name: label('取消分享')}).click();
  await a.getByRole('dialog').getByRole('button', {name: label('取消分享')}).click();
  await shareRow.getByText('已撤销', {exact: true}).waitFor();
  await c.getByRole('button', {name: label('导入分享')}).click();
  await c.getByPlaceholder('粘贴分享 Key').fill(shareKey);
  await c.getByRole('dialog').getByRole('button', {name: label('解析分享')}).click();
  await c.getByText('分享不存在、已失效或已被撤销', {exact: true}).waitFor();
  await c.getByRole('dialog').getByRole('button', {name: label('取消'), exact: true}).click();
  await c.getByRole('button', {name: `管理 ${filename}`, exact: true}).waitFor();
  pass('revocation rejects the original key while the recipient retains its imported copy');
  await a.goto(`${base}/ai`);
  const searchBox = a.getByPlaceholder('例如：上季度项目复盘中的风险'); await searchBox.waitFor();
  const deadline = Date.now() + 240000;
  while (true) {
    await searchBox.fill('海岚项目');
    const response = a.waitForResponse(r => new URL(r.url()).pathname === '/api/v1/search' && r.request().method() === 'POST');
    await a.getByRole('button', {name: label('搜索')}).click();
    const result = await response;
    assert.equal(result.status(), 200);
    const body = await result.json();
    if (body.hits.some(h => h.file.name === filename)) break;
    assert.ok(Date.now() < deadline, 'AI indexing did not complete');
    await new Promise(resolve => setTimeout(resolve, 4000));
  }
  await a.getByRole('button', {name: `打开 ${filename} 文件详情`, exact: true}).waitFor();
  await a.screenshot({path: `${output}/browser-search.png`, fullPage: true});
  pass('UI hybrid search retrieves the document through real AI, ES and Qdrant');
  await a.goto(`${base}/ask`);
  await a.getByRole('textbox', {name: '向文件提问'}).fill('海岚项目的恢复口令是什么？');
  const answerResponse = a.waitForResponse(r => new URL(r.url()).pathname === '/api/v1/ai/ask', {timeout: 150000});
  await a.getByRole('button', {name: label('提问')}).click();
  const answered = await answerResponse; assert.equal(answered.status(), 200);
  const answer = await answered.json(); assert.ok(answer.answer.includes('BROWSER915')); assert.ok(answer.citations.length > 0);
  await a.getByText(/BROWSER915/).first().waitFor();
  await a.screenshot({path: `${output}/browser-answer.png`, fullPage: true});
  pass('UI asks the real model and renders an answer grounded in file citations', {citations: answer.citations.length});
  await b.goto(`${base}/drive`);
  await b.getByRole('button', {name: '打开账户菜单', exact: true}).click();
  await b.getByRole('menuitem', {name: /退出登录/}).click();
  await b.waitForURL('**/login');
  await a.goto(`${base}/drive`); await a.getByRole('button', {name: `管理 ${filename}`, exact: true}).waitFor();
  pass('logging out one device leaves the other device authenticated');
  assert.equal(report.page_errors.length, 0); assert.equal(report.api_errors.length, 0);
  report.result = 'PASS';
} catch (error) {
  report.result = 'FAIL'; report.error = error.message; console.error(error.message);
  for (let i = 0; i < contexts.length; i++) {
    const page = contexts[i].pages()[0];
    if (page) {try {await page.screenshot({path: `${output}/browser-failure-${i}.png`, fullPage: true});} catch { /* Preserve the original failure. */ }}
  }
} finally {
  report.finished_at = new Date().toISOString(); save();
  await browser.close();
}
console.log(`browser: ${report.result} (${report.checks.length} checks)`);
process.exitCode = report.result === 'PASS' ? 0 : 1;
