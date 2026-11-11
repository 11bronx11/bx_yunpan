import { api } from './api';
import { searchFiles } from './ai';

jest.mock('./api', () => ({ api: jest.fn() }));

test('passes search cancellation to fetch and keeps scope filters in the JSON body', async () => {
  const controller = new AbortController();
  await searchFiles('query', 'fulltext', { folder_id: 'folder-1', signal: controller.signal });
  expect(api).toHaveBeenCalledWith('/api/v1/search', expect.objectContaining({ signal: controller.signal, method: 'POST' }));
  expect(JSON.parse(api.mock.calls[0][1].body)).toEqual({ query: 'query', mode: 'fulltext', include_subfolders: true, folder_id: 'folder-1' });
});
