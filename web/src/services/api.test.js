import { api } from './api';

test('uses the Chinese message for backend error codes', async () => {
  const fetchMock = jest.spyOn(global, 'fetch').mockResolvedValue({
    status: 409,
    ok: false,
    headers: { get: () => 'application/json' },
    json: async () => ({ error: { code: 'upload.file_exists', message: 'file already exists in drive' } }),
  });

  await expect(api('/api/v1/uploads', {}, false)).rejects.toMatchObject({
    code: 'upload.file_exists',
    message: '网盘中已存在相同内容，本次未重复上传',
  });

  fetchMock.mockRestore();
});

test('distinguishes a same-folder name conflict from duplicate content', async () => {
  const fetchMock = jest.spyOn(global, 'fetch').mockResolvedValue({
    status: 409,
    ok: false,
    headers: { get: () => 'application/json' },
    json: async () => ({ error: { code: 'upload.name_conflict', message: 'file name already exists in folder' } }),
  });

  await expect(api('/api/v1/uploads', {}, false)).rejects.toMatchObject({
    code: 'upload.name_conflict',
    message: '当前目录已存在同名文件，请重命名后上传',
  });

  fetchMock.mockRestore();
});

test('explains when the AI provider quota is exhausted', async () => {
  const fetchMock = jest.spyOn(global, 'fetch').mockResolvedValue({
    status: 429,
    ok: false,
    headers: { get: () => 'application/json' },
    json: async () => ({ error: { code: 'ai.quota_exhausted', message: 'AI provider quota exhausted' } }),
  });

  await expect(api('/api/v1/search', {}, false)).rejects.toMatchObject({
    code: 'ai.quota_exhausted',
    message: 'AI 模型免费额度已用尽，请切换可用模型或调整百炼计费设置',
  });

  fetchMock.mockRestore();
});
