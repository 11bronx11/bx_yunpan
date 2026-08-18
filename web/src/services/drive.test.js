import { api } from './api';
import { deleteFolder, moveFile, uploadFile, waitForUpload } from './drive';
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
