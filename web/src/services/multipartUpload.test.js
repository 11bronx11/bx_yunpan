import { api } from './api';
import { multipartUpload } from './multipartUpload';
import { uploadPart } from './uploadPart';

jest.mock('./api', () => ({ api: jest.fn(), clientError: code => Object.assign(new Error(code), { code }) }));
jest.mock('./uploadPart', () => ({ uploadPart: jest.fn() }));

const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const session = (count, id = 'window') => ({ id, status: 'uploading', part_size: 1, part_count: count, size_bytes: count, confirmed_parts: [] });
const file = count => new File(['x'.repeat(count)], 'window.bin');
const acknowledge = (path, options) => {
  const body = JSON.parse(options.body);
  if (path.endsWith('/presign')) return { parts: [...body.part_numbers].reverse().map(n => ({ part_number: n, url: `https://storage.test/${n}` })) };
  return { upload_id: path.split('/')[4], status: 'uploading', acknowledged_parts: body.parts };
};

beforeEach(() => { jest.clearAllMocks(); api.mockImplementation(acknowledge); uploadPart.mockResolvedValue('etag'); });

test('continuously fills a bounded window and confirms batches instead of individual parts', async () => {
  let active = 0, peak = 0, overlap = false, confirming = false;
  uploadPart.mockImplementation(async () => {
    active += 1; peak = Math.max(peak, active);
    if (confirming) overlap = true;
    await delay(1); active -= 1; return 'etag';
  });
  api.mockImplementation(async (path, options) => {
    if (path.includes('/confirm')) { confirming = true; await delay(5); confirming = false; }
    return acknowledge(path, options);
  });
  const result = await multipartUpload(file(100), session(100), jest.fn(), { policy: { confirmWait: 10000 } });
  expect(peak).toBe(4);
  expect(overlap).toBe(true);
  expect(result.session.confirmed_parts).toHaveLength(100);
  expect(api.mock.calls.filter(([p]) => p.endsWith('/presign'))).toHaveLength(5);
  expect(api.mock.calls.filter(([p]) => p.includes('/confirm'))).toHaveLength(13);
});

test('stops sending bytes when acknowledgements fill the unconfirmed window', async () => {
  let unblock;
  const blocked = new Promise(resolve => { unblock = resolve; });
  let first = true;
  api.mockImplementation(async (path, options) => {
    if (path.includes('/confirm') && first) { first = false; await blocked; }
    return acknowledge(path, options);
  });
  const result = multipartUpload(file(30), session(30), jest.fn(), {});
  await delay(50);
  expect(uploadPart).toHaveBeenCalledTimes(16);
  unblock();
  expect((await result).session.confirmed_parts).toHaveLength(30);
});

test('shares the global PUT budget between files', async () => {
  let active = 0, peak = 0;
  uploadPart.mockImplementation(async () => { active += 1; peak = Math.max(peak, active); await delay(2); active -= 1; return 'etag'; });
  await Promise.all(['a', 'b', 'c'].map(id => multipartUpload(file(12), session(12, id), jest.fn(), {})));
  expect(peak).toBe(8);
});

test('pause drains successful parts before returning a resumable session', async () => {
  let paused = false;
  uploadPart.mockImplementation(async () => { await delay(1); paused = true; return 'etag'; });
  const control = { isPaused: () => paused, pendingParts: new Map() };
  const result = await multipartUpload(file(25), session(25), jest.fn(), control);
  expect(result.paused).toBe(true);
  expect(result.session.confirmed_parts.length).toBeGreaterThan(0);
  expect(result.session.confirmed_parts.length).toBeLessThan(25);
  expect(control.pendingParts.size).toBe(0);
});

test('retries retained ETags after failed confirmation without uploading those bytes again', async () => {
  const retained = new Map();
  api.mockImplementation((path, options) => {
    if (path.includes('/confirm')) throw Object.assign(new Error('unavailable'), { status: 503 });
    return acknowledge(path, options);
  });
  await expect(multipartUpload(file(8), session(8), jest.fn(), { pendingParts: retained, policy: { attempts: 1 } })).rejects.toThrow('unavailable');
  expect(retained.size).toBe(8);
  uploadPart.mockClear(); api.mockImplementation(acknowledge);
  const result = await multipartUpload(file(8), session(8), jest.fn(), { pendingParts: retained });
  expect(result.session.confirmed_parts).toHaveLength(8);
  expect(uploadPart).not.toHaveBeenCalled();
});
