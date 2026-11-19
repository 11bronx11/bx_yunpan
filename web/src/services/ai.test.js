import { api } from './api';
import { listAskableFiles, searchFiles } from './ai';

jest.mock('./api', () => ({ api: jest.fn() }));

test('passes search cancellation to fetch and keeps scope filters in the JSON body', async () => {
  const controller = new AbortController();
  await searchFiles('query', 'fulltext', { folder_id: 'folder-1', signal: controller.signal });
  expect(api).toHaveBeenCalledWith('/api/v1/search', expect.objectContaining({ signal: controller.signal, method: 'POST' }));
  expect(JSON.parse(api.mock.calls[0][1].body)).toEqual({ query: 'query', mode: 'fulltext', include_subfolders: true, folder_id: 'folder-1' });
});

test('sends an explicit false to exclude subfolders from a selected directory', async () => {
  await listAskableFiles({ folder_id: 'folder-1', include_subfolders: false });
  const params = new URLSearchParams(api.mock.calls.at(-1)[0].split('?')[1]);
  expect(params.get('folder_id')).toBe('folder-1');
  expect(params.get('include_subfolders')).toBe('false');
});
