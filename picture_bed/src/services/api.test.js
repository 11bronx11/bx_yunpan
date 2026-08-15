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
    message: '网盘中已存在该文件，本次未重复上传',
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
