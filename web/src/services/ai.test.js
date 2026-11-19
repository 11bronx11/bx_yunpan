import { api } from './api';
import { askFiles, listAskableFiles, searchFiles } from './ai';

jest.mock('./api', () => ({ api: jest.fn() }));

test('passes ask cancellation to fetch without serializing its signal into the request', async () => {
  const controller = new AbortController();
  await askFiles('question', { scope: { mode: 'selected', file_ids: ['file-1'] }, signal: controller.signal });
  const [path, options] = api.mock.calls.at(-1);
  expect(path).toBe('/api/v1/ai/ask');
  expect(options.signal).toBe(controller.signal);
  expect(JSON.parse(options.body)).toEqual({ question: 'question', scope: { mode: 'selected', file_ids: ['file-1'] } });
});

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
