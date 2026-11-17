// ASYNC_PLAYWRIGHT_MODULE=<runner>/node_modules/playwright node this-file OUTPUT
const fs = require('node:fs/promises');
const path = require('node:path');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const { chromium } = require(process.env.ASYNC_PLAYWRIGHT_MODULE || 'playwright');

async function main() {
  const output = path.resolve(process.argv[2]);
  const fixture = JSON.parse(await fs.readFile(path.join(output, 'browser-fixture.json')));
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, acceptDownloads: true });
  await context.tracing.start({ screenshots: true, snapshots: true });
  const page = await context.newPage();
  page.setDefaultTimeout(30000);
  const result = { status: 'FAIL', puts: [], signatures: [], confirmations: [], page_errors: [] };
  const inflight = new Set();
  let peak = 0, hold = true;
  const payload = crypto.randomBytes(40 * 1024 * 1024 + 123);
  const name = '窗口续传-' + Date.now() + '.bin';
  const payloadFile = path.join(output, name);
  await fs.writeFile(payloadFile, payload);
  const isPut = request => request.method() === 'PUT' && new URL(request.url()).pathname.startsWith('/storage/');
  page.on('pageerror', error => result.page_errors.push(error.message));
  page.on('request', request => {
    if (isPut(request)) { inflight.add(request); peak = Math.max(peak, inflight.size); }
    if (request.url().includes('/parts/presign')) result.signatures.push(request.postDataJSON().part_numbers);
    if (request.url().includes('/parts/confirm')) result.confirmations.push(request.postDataJSON().parts.map(p => p.part_number));
  });
  page.on('requestfinished', request => inflight.delete(request));
  page.on('requestfailed', request => inflight.delete(request));
  page.on('response', response => {
    if (isPut(response.request())) result.puts.push({ part: Number(new URL(response.url()).searchParams.get('partNumber')), status: response.status() });
  });
  await page.route('**/storage/**', async route => {
    if (hold && route.request().method() === 'PUT') await new Promise(resolve => setTimeout(resolve, 1200));
    await route.continue();
  });
  try {
    await page.goto(fixture.base_url, { waitUntil: 'networkidle' });
    await page.getByPlaceholder('用户名', { exact: true }).fill(fixture.username);
    await page.getByPlaceholder('密码', { exact: true }).fill(fixture.password);
    await page.getByRole('button', { name: /Enter cloud/ }).click();
    await page.waitForURL('**/drive');
    await page.locator('input[type=file]').setInputFiles(payloadFile);
    await page.getByRole('link', { name: /传输/ }).click();
    await page.waitForFunction(() => document.body.textContent.includes('上传中'));
    const deadline = Date.now() + 30000;
    while (peak < 4 && Date.now() < deadline) await page.waitForTimeout(50);
    assert.equal(peak, 4, 'single file should have four PUTs in flight');
    await page.getByRole('button', { name: '暂停 ' + name, exact: true }).click();
    await page.getByText('已暂停', { exact: true }).waitFor();
    assert.equal(result.puts.length, 4);
    assert(result.confirmations.some(parts => parts.length > 1));
    await page.screenshot({ path: path.join(output, 'upload-paused.png'), fullPage: true });
    await page.reload({ waitUntil: 'networkidle' });
    await page.getByText('上传已中断', { exact: true }).waitFor();
    hold = false;
    const completed = page.waitForResponse(async response => {
      if (response.request().method() !== 'GET' || !/\/api\/v1\/uploads\/[a-f0-9-]+$/.test(new URL(response.url()).pathname)) return false;
      const session = await response.json();
      return session.filename === name && session.status === 'completed';
    }, { timeout: 90000 });
    await page.locator('input[type=file]').setInputFiles(payloadFile);
    const session = await (await completed).json();
    await page.getByText('已完成', { exact: true }).waitFor();
    assert.equal(result.puts.length, 5, 'resume should upload only the missing fifth part');
    assert.deepEqual(result.puts.map(p => p.part).sort((a, b) => a - b), [1, 2, 3, 4, 5]);
    assert(result.puts.every(p => p.status === 200));
    await page.screenshot({ path: path.join(output, 'upload-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: path.join(output, 'upload-mobile.png'), fullPage: true });
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.locator('a[href="/drive"]').first().click();
    const row = page.getByRole('row', { name: '文件 ' + name, exact: true });
    await row.waitFor();
    await row.hover();
    const downloaded = page.waitForEvent('download');
    await row.getByRole('button', { name: '下载 ' + name, exact: true }).click();
    const download = await downloaded;
    const destination = path.join(output, 'browser-download.bin');
    await download.saveAs(destination);
    const actual = await fs.readFile(destination);
    const sha = value => crypto.createHash('sha256').update(value).digest('hex');
    assert.equal(sha(actual), sha(payload));
    assert.deepEqual(result.page_errors, []);
    Object.assign(result, { status: 'PASS', peak_puts: peak, bytes: payload.length, sha256: sha(actual), session_id: session.id });
  } catch (error) {
    result.error = error.stack;
    await page.screenshot({ path: path.join(output, 'upload-failure.png'), fullPage: true }).catch(() => {});
    process.exitCode = 1;
  } finally {
    await context.tracing.stop({ path: path.join(output, 'upload-trace.zip') });
    await fs.writeFile(path.join(output, 'browser-results.json'), JSON.stringify(result, null, 2));
    await fs.writeFile(path.join(output, 'browser-complete.json'), JSON.stringify({ status: result.status }));
    await browser.close();
    console.log(JSON.stringify(result));
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
