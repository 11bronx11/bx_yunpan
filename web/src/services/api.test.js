import { api, onSessionExpired, setAccessToken } from './api';

test('explains invalid share parameters without reporting a file conflict', async () => {
  const fetchMock = jest.spyOn(global, 'fetch').mockResolvedValue({
    status: 422,
    ok: false,
    headers: { get: () => 'application/json' },
    json: async () => ({ error: { code: 'share.invalid_input', message: 'invalid share request' } }),
  });
  try {
    await expect(api('/api/v1/shares', {}, false)).rejects.toMatchObject({
      status: 422, code: 'share.invalid_input', message: '分享信息不符合要求，请检查后重试',
    });
  } finally {
    fetchMock.mockRestore();
  }
});

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

test('shows a generic unavailable message when the AI provider quota is exhausted', async () => {
  const fetchMock = jest.spyOn(global, 'fetch').mockResolvedValue({
    status: 429,
    ok: false,
    headers: { get: () => 'application/json' },
    json: async () => ({ error: { code: 'ai.quota_exhausted', message: 'AI provider quota exhausted' } }),
  });

  await expect(api('/api/v1/search', {}, false)).rejects.toMatchObject({
    code: 'ai.quota_exhausted',
    message: '功能暂时不可用',
  });

  fetchMock.mockRestore();
});

test('keeps the HTTP error when refresh returns an HTML gateway failure', async () => {
  const fetchMock = jest.spyOn(global, 'fetch')
    .mockResolvedValueOnce({ status: 401 })
    .mockResolvedValueOnce({ status: 502, ok: false, headers: { get: () => 'text/html' }, text: async () => '<html>Bad Gateway</html>' });
  try {
    await expect(api('/api/v1/folders/root')).rejects.toMatchObject({ status: 502, message: '服务暂时不可用' });
  } finally { fetchMock.mockRestore(); }
});

test('concurrent unauthorized requests share one refresh and retry with the new token', async () => {
  let finishRefresh;
  const ok = value => ({ status: 200, ok: true, headers: { get: () => 'application/json' }, json: async () => value });
  setAccessToken('expired');
  const fetchMock = jest.spyOn(global, 'fetch').mockImplementation(async (path, options) => {
    if (path === '/api/v1/auth/refresh') return new Promise(resolve => { finishRefresh = () => resolve(ok({ access_token: 'renewed' })); });
    if (options.headers.get('Authorization') === 'Bearer expired') return { status: 401 };
    return ok({ id: path });
  });
  try {
    const first = api('/first'), second = api('/second');
    await Promise.resolve();
    finishRefresh();
    await expect(Promise.all([first, second])).resolves.toEqual([{ id: '/first' }, { id: '/second' }]);
    expect(fetchMock.mock.calls.filter(([path]) => path === '/api/v1/auth/refresh')).toHaveLength(1);
  } finally { setAccessToken(null); fetchMock.mockRestore(); }
});

test('a rejected refresh clears authentication and notifies the UI', async () => {
  const expired = jest.fn();
  const unsubscribe = onSessionExpired(expired);
  setAccessToken('expired');
  const fetchMock = jest.spyOn(global, 'fetch')
    .mockResolvedValueOnce({ status: 401 })
    .mockResolvedValueOnce({ status: 401, ok: false, headers: { get: () => 'application/json' }, json: async () => ({ error: { code: 'auth.unauthorized' } }) })
    .mockResolvedValueOnce({ status: 204 });
  try {
    await expect(api('/api/v1/folders/root')).rejects.toMatchObject({ status: 401 });
    expect(expired).toHaveBeenCalledTimes(1);
    await api('/probe', {}, false);
    expect(fetchMock.mock.calls[2][1].headers.has('Authorization')).toBe(false);
  } finally { unsubscribe(); setAccessToken(null); fetchMock.mockRestore(); }
});

test('a gateway failure during refresh keeps the session available for retry', async () => {
  const expired = jest.fn();
  const unsubscribe = onSessionExpired(expired);
  const fetchMock = jest.spyOn(global, 'fetch')
    .mockResolvedValueOnce({ status: 401 })
    .mockResolvedValueOnce({ status: 503, ok: false, headers: { get: () => 'text/html' }, text: async () => '<html>Unavailable</html>' });
  try {
    await expect(api('/api/v1/folders/root')).rejects.toMatchObject({ status: 503 });
    expect(expired).not.toHaveBeenCalled();
  } finally { unsubscribe(); fetchMock.mockRestore(); }
});
