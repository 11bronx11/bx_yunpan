import { api } from './api';

export const listShares = ({ cursor = '', limit = 50, signal } = {}) => {
  const params = new URLSearchParams({ limit: String(limit) });
  if (cursor) params.set('cursor', cursor);
  return api(`/api/v1/shares?${params}`, { signal });
};
export const getShare = shareId => api(`/api/v1/shares/${shareId}`);
export const updateShare = (shareId, version, expiresAt) => api(`/api/v1/shares/${shareId}`, { method: 'PATCH', body: JSON.stringify({ version, expires_at: expiresAt }) });
export const createShare = (fileId, expiresAt = null) => api('/api/v1/shares', { method: 'POST', body: JSON.stringify({ file_id: fileId, expires_at: expiresAt }) });
export const revokeShare = shareId => api(`/api/v1/shares/${shareId}`, { method: 'DELETE' });
export const resolveShare = (shareKey, { signal } = {}) => api('/api/v1/public/shares/resolve', { method: 'POST', body: JSON.stringify({ share_key: shareKey }), signal }, false);
export const getSharedContent = token => api('/api/v1/public/shares/content', { headers: { 'X-Share-Token': token } }, false);
export const importShare = (shareId, folderId, token, idempotencyKey = crypto.randomUUID()) => api(`/api/v1/shares/${shareId}/import`, { method: 'POST', headers: { 'Idempotency-Key': idempotencyKey, 'X-Share-Token': token }, body: JSON.stringify({ target_folder_id: folderId }) });
