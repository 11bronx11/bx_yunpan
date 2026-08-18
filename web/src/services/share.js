import { api } from './api';

export const listShares = () => api('/api/v1/shares');
export const createShare = (fileId, expiresAt = null) => api('/api/v1/shares', { method: 'POST', body: JSON.stringify({ file_id: fileId, expires_at: expiresAt }) });
export const revokeShare = shareId => api(`/api/v1/shares/${shareId}`, { method: 'DELETE' });
export const resolveShare = shareKey => api('/api/v1/public/shares/resolve', { method: 'POST', body: JSON.stringify({ share_key: shareKey }) }, false);
export const getSharedContent = token => api('/api/v1/public/shares/content', { headers: { 'X-Share-Token': token } }, false);
export const importShare = (shareId, folderId, token) => api(`/api/v1/shares/${shareId}/import`, { method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID(), 'X-Share-Token': token }, body: JSON.stringify({ target_folder_id: folderId }) });
