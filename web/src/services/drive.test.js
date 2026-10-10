import { api } from './api';
import { deleteFolder, getChildren, moveFile, uploadFile, waitForUpload } from './drive';
import { hashFile } from './fileHash';

jest.mock('./api', () => ({
  api: jest.fn(),
  clientError: (code, fallback) => {
    const error = new Error(fallback || code);
    error.code = code;
    return error;
  },
}));

jest.mock('./fileHash', () => ({
  hashFile: jest.fn(() => Promise.resolve('a'.repeat(64))),
}));

const hash = 'a'.repeat(64);

test('encodes directory pagination and forwards request cancellation', async () => {
  api.mockResolvedValue({ items: [], page: { has_more: false } });
  const controller = new AbortController();
  await getChildren('folder-1', { cursor: 'a+b/c=', limit: 50, sort: 'name_desc', signal: controller.signal });
  const [path, options] = api.mock.calls.at(-1);
  const url = new URL(path, 'http://localhost');
  expect(url.searchParams.get('cursor')).toBe('a+b/c=');
  expect(url.searchParams.get('limit')).toBe('50');
  expect(url.searchParams.get('sort')).toBe('name_desc');
  expect(options.signal).toBe(controller.signal);
});

test('resume skips confirmed parts and uploads only missing parts', async () => {
  const fetchMock = jest.spyOn(global, 'fetch').mockResolvedValue({
    ok: true,
    headers: { get: () => '"etag-2"' },
  });
  const session = {
    id: 'upload-1',
    status: 'uploading',
    folder_id: 'folder-1',
    filename: 'resume.bin',
    sha256: hash,
    size_bytes: 8,
    part_size: 4,
    part_count: 2,
    confirmed_parts: [{ part_number: 1, size_bytes: 4 }],
  };
  hashFile.mockResolvedValue(session.sha256);
  api.mockImplementation(async (path, options = {}) => {
    if (path.endsWith('/parts/presign')) return { parts: [{ part_number: 2, url: 'https://storage.test/part-2' }] };
    if (path.endsWith('/parts/confirm')) return { ...session, confirmed_parts: [{ part_number: 1, size_bytes: 4 }, { part_number: 2, size_bytes: 4 }] };
    if (path.endsWith('/complete')) return { ...session, status: 'verifying' };
    if (path === '/api/v1/uploads/upload-1') return { ...session, status: 'completed', completed_entry_id: 'file-1' };
    throw new Error(`unexpected API call: ${path} ${options.method || 'GET'}`);
  });

  const file = new File(['12345678'], 'resume.bin', { type: 'application/octet-stream' });
  const result = await uploadFile(file, 'folder-1', jest.fn(), {}, session);

  const presignCalls = api.mock.calls.filter(([path]) => path.endsWith('/parts/presign'));
  expect(presignCalls).toHaveLength(1);
  expect(JSON.parse(presignCalls[0][1].body)).toEqual({ part_numbers: [2] });
  expect(fetchMock).toHaveBeenCalledTimes(1);
  expect(result.file.id).toBe('file-1');

	fetchMock.mockRestore();
});

test('server verification keeps polling beyond the old timeout', async () => {
	const session = { id: 'upload-long', status: 'verifying', filename: 'large.bin' };
	let polls = 0;
	api.mockImplementation(async path => {
		if (path !== '/api/v1/uploads/upload-long') throw new Error(`unexpected API call: ${path}`);
		polls += 1;
		if (polls <= 60) return session;
		return { ...session, status: 'completed', completed_entry_id: 'file-large' };
	});

	const result = await waitForUpload(session, jest.fn(), {}, 0);

	expect(polls).toBe(61);
	expect(result.file.id).toBe('file-large');
});

test('asynchronous create waits for initialization before signing parts', async () => {
  const session = { id: 'async-upload', status: 'initializing', filename: 'async.txt', part_size: 3, part_count: 1 };
  let initialized = false;
  let initializedAtPresign = false;
  let completing = false;
  const fetchMock = jest.spyOn(global, 'fetch').mockResolvedValue({ ok: true, headers: { get: () => 'etag' } });
  api.mockImplementation(async path => {
    if (path === '/api/v1/uploads') return { mode: 'multipart', upload: session };
    if (path === '/api/v1/uploads/async-upload') {
      initialized = true;
      return { ...session, status: completing ? 'completed' : 'created', completed_entry_id: 'async-file' };
    }
    if (path.endsWith('/parts/presign')) {
      initializedAtPresign = initialized;
      return { parts: [{ url: 'https://storage.test/part' }] };
    }
    if (path.endsWith('/parts/confirm')) return { ...session, status: 'uploading' };
    if (path.endsWith('/complete')) { completing = true; return { ...session, status: 'completing' }; }
    throw new Error(path);
  });
  const result = await uploadFile(new File(['abc'], 'async.txt'), 'folder-1', jest.fn(), { pollInterval: 0 });
  expect(result.file.id).toBe('async-file');
  expect(initializedAtPresign).toBe(true);
  fetchMock.mockRestore();
});

test('resuming completion polls without requiring the original file', async () => {
  api.mockResolvedValue({ id: 'complete-upload', status: 'completed', completed_entry_id: 'complete-file' });
  const result = await uploadFile(null, 'folder', jest.fn(), {}, { id: 'complete-upload', status: 'completing' });
  expect(result.file.id).toBe('complete-file');
});

test('moves a file with optimistic concurrency metadata', async () => {
	api.mockResolvedValue({ id: 'file-1', folder_id: 'folder-2', version: 4 });

	await moveFile('file-1', 3, 'folder-2');

	expect(api).toHaveBeenLastCalledWith('/api/v1/files/file-1/move', {
		method: 'POST',
		headers: { 'If-Match': '3' },
		body: JSON.stringify({ target_folder_id: 'folder-2' }),
	});
});

test('deletes a folder with optimistic concurrency metadata', async () => {
	api.mockResolvedValue(null);

	await deleteFolder('folder-1', 2);

	expect(api).toHaveBeenLastCalledWith('/api/v1/folders/folder-1', {
		method: 'DELETE',
		headers: { 'If-Match': '2' },
	});
});
