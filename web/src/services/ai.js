import { api } from './api';

const randomId = () => window.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(16).slice(2)}`;

export const searchFiles = (query, mode = 'hybrid', options = {}) => api('/api/v1/search', { method: 'POST', body: JSON.stringify({ query, mode, include_subfolders: true, ...options }) });
export const askFiles = (question, options = {}) => api('/api/v1/ai/ask', { method: 'POST', body: JSON.stringify({ question, ...options }) });
export const getFileAI = (fileId, options = {}) => api(`/api/v1/files/${fileId}/ai`, options);
export const reprocessFileAI = fileId => api(`/api/v1/files/${fileId}/ai/reprocess`, { method: 'POST', headers: { 'Idempotency-Key': randomId() } });
export const getAIJob = (taskId, options = {}) => api(`/api/v1/ai/jobs/${taskId}`, options);
