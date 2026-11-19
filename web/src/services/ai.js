import { api } from './api';

const randomId = () => window.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(16).slice(2)}`;

export const searchFiles = (query, mode = 'hybrid', { signal, ...options } = {}) => api('/api/v1/search', { method: 'POST', signal, body: JSON.stringify({ query, mode, include_subfolders: true, ...options }) });
export const askFiles = (question, options = {}) => api('/api/v1/ai/ask', { method: 'POST', body: JSON.stringify({ question, ...options }) });
export const getFileAI = (fileId, options = {}) => api(`/api/v1/files/${fileId}/ai`, options);
export const reprocessFileAI = (fileId, targets = ['all']) => api(`/api/v1/files/${fileId}/ai/reprocess`, { method: 'POST', headers: { 'Idempotency-Key': randomId() }, body: JSON.stringify({ targets }) });
export const getAIJob = (taskId, options = {}) => api(`/api/v1/ai/jobs/${taskId}`, options);

export const listAskableFiles = ({ signal, mime_types = [], ...options } = {}) => {
  const query = new URLSearchParams({ eligibility: 'askable', limit: '30' });
  Object.entries(options).forEach(([key, value]) => { if (value !== undefined && value !== null && value !== '') query.set(key, String(value)); });
  mime_types.forEach(value => query.append('mime_types', value));
  return api(`/api/v1/ai/files?${query}`, { signal });
};
