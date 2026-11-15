// npm install --prefix <runner> --ignore-scripts playwright@1.61.1
// ASYNC_PLAYWRIGHT_MODULE=<runner>/node_modules/playwright node this-file OUTPUT
// OUTPUT is the async-process.py output directory with browser-fixture.json.
const fs = require('node:fs/promises');
const path = require('node:path');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const playwright = require(process.env.ASYNC_PLAYWRIGHT_MODULE || 'playwright');

async function main() {
  const output = path.resolve(process.argv[2]);
  const fixture = JSON.parse(await fs.readFile(path.join(output, 'browser-fixture.json')));
  const browser = await playwright.chromium.launch({ headless: true });
  const result = { browser: await browser.version(), playwright: '1.61.1', runs: [] };
  try {
    for (let run = 0; run < 3; run++) {
      const context = await browser.newContext({ viewport: run === 2 ? { width: 390, height: 844 } : { width: 1440, height: 1000 }, acceptDownloads: true });
      await context.tracing.start({ screenshots: true, snapshots: true });
      const page = await context.newPage();
      const errors = [], puts = [], uploads = [];
      page.on('pageerror', error => errors.push(error.message));
      page.on('response', response => {
        const req = response.request();
        if (req.method() === 'PUT') puts.push({ path: new URL(req.url()).pathname, status: response.status() });
      });
      await page.goto(fixture.base_url, { waitUntil: 'networkidle' });
      await page.getByPlaceholder('用户名', { exact: true }).fill(fixture.username);
      await page.getByPlaceholder('密码', { exact: true }).fill(fixture.password);
      await page.getByRole('button', { name: /Enter cloud/ }).click();
      await page.waitForURL('**/drive');
      await page.getByRole('button', { name: /新建目录/ }).click();
      const dirname = 'browser-' + run + '-' + crypto.randomBytes(3).toString('hex');
      await page.getByRole('textbox', { name: '目录名称' }).fill(dirname);
      await page.getByRole('button', { name: /创\s*建/ }).click();
      await page.getByRole('row', { name: new RegExp(dirname) }).waitFor();
      const name = '真实上传-' + run + '.bin';
      const payload = crypto.randomBytes(8193 + run);
      const completed = new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('upload did not complete within 60 seconds')), 60000);
        const onResponse = async response => {
          if (!/\/api\/v1\/uploads\/[a-f0-9-]+$/.test(new URL(response.url()).pathname) || response.request().method() !== 'GET') return;
          try {
            const session = await response.json();
            uploads.push({ id: session.id, status: session.status });
            if (session.filename === name && session.status === 'completed') {
              clearTimeout(timer);
              page.off('response', onResponse);
              resolve(session);
            }
          } catch (error) { /* A navigated request can be aborted. */ }
        };
        page.on('response', onResponse);
      });
      await page.locator('input[type=file]').setInputFiles({ name, mimeType: 'application/octet-stream', buffer: payload });
      const session = await completed;
      await page.goto(fixture.base_url + '/drive');
      const row = page.getByRole('row', { name: '文件 ' + name, exact: true });
      await row.waitFor();
      await row.hover();
      const downloadEvent = page.waitForEvent('download');
      if (run === 2) {
        await row.getByRole('button', { name: '管理 ' + name, exact: true }).click();
        await page.getByRole('menuitem', { name: /下载/ }).click();
      } else {
        await row.getByRole('button', { name: '下载 ' + name, exact: true }).click();
      }
      const download = await downloadEvent;
      const file = path.join(output, `browser-download-${run}.bin`);
      await download.saveAs(file);
      const actual = await fs.readFile(file);
      assert.equal(crypto.createHash('sha256').update(actual).digest('hex'), crypto.createHash('sha256').update(payload).digest('hex'));
      await page.reload({ waitUntil: 'networkidle' });
      await page.getByRole('row', { name: '文件 ' + name, exact: true }).waitFor();
      const second = await context.newPage();
      await second.goto(fixture.base_url + '/drive', { waitUntil: 'networkidle' });
      await second.getByRole('row', { name: '文件 ' + name, exact: true }).waitFor();
      assert(puts.length >= 1 && puts.every(x => x.path.startsWith('/storage/') && x.status === 200));
      assert.deepEqual(errors, []);
      await page.screenshot({ path: path.join(output, `browser-${run}.png`), fullPage: true });
      result.runs.push({ run, viewport: page.viewportSize(), status: 'PASS', session_id: session.id, file_id: session.completed_entry_id,
        sha256: crypto.createHash('sha256').update(actual).digest('hex'), storage_puts: puts, upload_states: uploads, page_errors: errors });
      await context.tracing.stop({ path: path.join(output, `browser-${run}-trace.zip`) });
      await context.close();
    }
    result.status = 'PASS';
  } catch (error) {
    result.status = 'FAIL';
    result.error = error.stack;
    for (const context of browser.contexts()) {
      for (const page of context.pages()) await page.screenshot({ path: path.join(output, 'browser-failure.png') }).catch(() => {});
      await context.tracing.stop({ path: path.join(output, 'browser-failure-trace.zip') }).catch(() => {});
    }
    process.exitCode = 1;
  } finally {
    await fs.writeFile(path.join(output, 'browser-results.json'), JSON.stringify(result, null, 2));
    await fs.writeFile(path.join(output, 'browser-complete.json'), JSON.stringify({ status: result.status }));
    await browser.close();
    console.log(JSON.stringify(result));
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
