let accessToken = null;
let refreshPromise = null;
const sessionExpiredListeners = new Set();

export const onSessionExpired = listener => {
  sessionExpiredListeners.add(listener);
  return () => sessionExpiredListeners.delete(listener);
};

const errorMessages = {
  'auth.unauthorized': '登录状态已失效，请重新登录',
  'auth.invalid_credentials': '账号或密码不正确',
  'identity.conflict': '用户名或邮箱已被使用',
  'identity.invalid_input': '注册信息不符合要求，请检查后重试',
  'drive.not_found': '文件或目录不存在，可能已被删除',
  'folder.not_empty': '目录中仍有内容，暂时不能删除',
  'drive.conflict': '名称已存在，或内容已被其他操作修改',
  'drive.invalid_input': '文件或目录操作不合法',
  'drive.invalid_cursor': '目录分页参数无效，请刷新目录',
  'drive.cursor_stale': '目录内容已变化，请刷新目录',
  'upload.not_found': '上传任务不存在或已经过期',
  'upload.file_exists': '网盘中已存在相同内容，本次未重复上传',
  'upload.name_conflict': '当前目录已存在同名文件，请重命名后上传',
  'upload.part_conflict': '分片确认与服务器记录冲突，请重新创建上传任务',
  'client.upload_timeout': '上传请求超时，请稍后重试',
  'client.upload_part': '分片上传失败，请重试',
  'client.upload_etag': '未能取得分片确认信息，请重试',
  'client.upload_response': '上传确认响应异常，请重试',
  'upload.conflict': '上传状态已发生变化，请刷新后重试',
  'upload.invalid_input': '上传信息不完整，请重新选择文件',
  'upload.hash_mismatch': '文件完整性校验失败，请重新上传',
  'upload.storage_unavailable': '对象存储空间不足，上传未完成，请扩容或清理空间后重试',
  'upload.verification_failed': '服务器校验失败，请稍后重新上传',
  'quota.exceeded': '网盘剩余空间不足，无法保存该文件',
  'share.not_found': '分享不存在、已失效或已被撤销',
  'share.invalid_input': '分享信息不符合要求，请检查后重试',
  'share.conflict': '网盘中已存在该文件，无法重复导入',
  'share.inactive': '分享已失效，仅可查看，请刷新列表',
  'share.version_conflict': '分享已被其他操作修改，请刷新后重试',
  'ai.invalid_scope': '请选择 1 到 50 个有效文件',
  'ai.scope_invalid': '部分文件已删除或尚未准备好，请更新选择后重试',
  'ai.scope_changed': '文件范围已变化，请更新选择后重试',
  'ai.semantic_unavailable': '语义检索暂不可用，可使用全文检索',
  'ai.answer_unavailable': '回答服务暂不可用，请稍后重试',
  'ai.query_timeout': '本次查询超时，请稍后重试',
  'ai.scope_too_large': '本次文件范围过大，请选择更小的范围',
  'ai.capability_disabled': '当前配置未启用此能力',
  'ai.capability_unsupported': '当前文件类型不支持此能力',
  'ai.not_found': '没有找到对应的 AI 处理结果',
  'ai.invalid_request': '检索或提问内容不符合要求',
  'ai.quota_exhausted': '功能暂时不可用',
  'ai.rate_limited': 'AI 请求过于频繁，请稍后再试',
  'ai.unavailable': '功能暂时不可用',
  'route.not_found': '请求的服务接口不存在',
  'internal.error': '服务暂时异常，请稍后重试',
  'client.network': '网络连接失败，请检查网络后重试',
  'client.file_mismatch': '所选文件与原上传文件不一致，请重新选择',
  'client.hash_failed': '文件校验失败，请重新选择文件后重试',
};

const statusMessages = {
  400: '请求内容有误，请检查后重试',
  401: '登录状态已失效，请重新登录',
  403: '没有权限执行此操作',
  404: '请求的内容不存在',
  409: '当前操作与已有内容冲突',
  413: '文件过大，无法上传',
  422: '输入内容不符合要求，请检查后重试',
  429: '操作过于频繁，请稍后重试',
  500: '服务暂时异常，请稍后重试',
  502: '服务暂时不可用',
  503: '服务暂时不可用',
  504: '请求处理超时，请稍后重试',
};

export const errorMessage = (code, status, fallback) => errorMessages[code]
  || statusMessages[status]
  || fallback
  || '请求失败，请稍后重试';

export const clientError = (code, fallback) => {
  const error = new Error(errorMessage(code, null, fallback));
  error.code = code;
  return error;
};

export const setAccessToken = token => {
  accessToken = token || null;
};

const parseResponse = async response => {
  if (response.status === 204) return null;
  const type = response.headers.get('content-type') || '';
  const data = type.includes('json') ? await response.json() : await response.text();
  if (!response.ok) {
    const code = data?.error?.code;
    const error = new Error(errorMessage(code, response.status));
    error.status = response.status;
    const retryAfter = response.headers.get('Retry-After');
    error.retryAfter = /^\d+$/.test(retryAfter || '') ? Number(retryAfter) * 1000 : Math.max(0, Date.parse(retryAfter) - Date.now()) || 0;
    error.code = code;
    error.details = data?.error?.details;
    error.invalidFiles = data?.invalid_files || [];
    throw error;
  }
  return data;
};

const refreshAccess = async () => {
  if (!refreshPromise) {
    refreshPromise = fetch('/api/v1/auth/refresh', {
      method: 'POST',
      credentials: 'include',
    }).then(parseResponse).then(session => {
      setAccessToken(session.access_token);
      return session;
    }).catch(error => {
      if (error.status === 401) {
        setAccessToken(null);
        sessionExpiredListeners.forEach(listener => listener());
      }
      if (error.status || error.code) throw error;
      throw clientError('client.network');
    }).finally(() => {
      refreshPromise = null;
    });
  }
  return refreshPromise;
};

export const api = async (path, options = {}, retry = true) => {
  const headers = new Headers(options.headers || {});
  if (options.body && !(options.body instanceof FormData) && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (accessToken) headers.set('Authorization', `Bearer ${accessToken}`);
  let response;
  try {
    response = await fetch(path, { ...options, headers, credentials: 'include' });
  } catch (error) {
    if (error.name === 'AbortError') throw error;
    throw clientError('client.network');
  }
  if (response.status === 401 && retry && path !== '/api/v1/auth/refresh') {
    await refreshAccess();
    return api(path, options, false);
  }
  return parseResponse(response);
};

export const restoreSession = refreshAccess;
