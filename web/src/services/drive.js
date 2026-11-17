import { api, clientError } from './api';
import { multipartUpload } from './multipartUpload';
import { uploadRequest } from './uploadRequest';
import { uploadScheduler } from './uploadScheduler';
import { hashFile } from './fileHash';
import { createPollDelay, sleep } from './polling';

export const getRoot = ({ signal } = {}) => api('/api/v1/folders/root', { signal });
export const getChildren = (folderId, { cursor, limit, sort, signal } = {}) => {
  const params = new URLSearchParams();
  if (cursor) params.set('cursor', cursor);
  if (limit) params.set('limit', String(limit));
  if (sort) params.set('sort', sort);
  const query = params.toString();
  return api(`/api/v1/folders/${folderId}/children${query ? `?${query}` : ''}`, { signal });
};
export const getBreadcrumb = (folderId, { signal } = {}) => api(`/api/v1/folders/${folderId}/breadcrumb`, { signal });
export const createFolder = (parentId, name) => api('/api/v1/folders', { method: 'POST', body: JSON.stringify({ parent_id: parentId, name }) });
export const renameFolder = (id, version, name) => api(`/api/v1/folders/${id}`, { method: 'PATCH', headers: { 'If-Match': String(version) }, body: JSON.stringify({ name }) });
export const deleteFolder = (id, version) => api(`/api/v1/folders/${id}`, { method: 'DELETE', headers: { 'If-Match': String(version) } });
export const renameFile = (id, version, name) => api(`/api/v1/files/${id}`, { method: 'PATCH', headers: { 'If-Match': String(version) }, body: JSON.stringify({ name }) });
export const moveFile = (id, version, targetFolderId) => api(`/api/v1/files/${id}/move`, { method: 'POST', headers: { 'If-Match': String(version) }, body: JSON.stringify({ target_folder_id: targetFolderId }) });
export const deleteFile = (id, version) => api(`/api/v1/files/${id}`, { method: 'DELETE', headers: { 'If-Match': String(version) } });
export const getDownloadURL = id => api(`/api/v1/files/${id}/download-url`);
export const getPreview = (id, { signal } = {}) => api(`/api/v1/files/${id}/preview`, { signal });

const randomId = () => window.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(16).slice(2)}`;

export const listActiveUploads = () => api('/api/v1/uploads?status=active');
export const abortUpload = (uploadId, control = {}) => uploadRequest(`/api/v1/uploads/${uploadId}`, { method: 'DELETE' }, control);
export const getUpload = (uploadId, control = {}) => uploadRequest(`/api/v1/uploads/${uploadId}`, {}, control);

const validateResumeFile = async (file, session, signal) => {
  if (file.name !== session.filename || file.size !== session.size_bytes) {
    throw clientError('client.file_mismatch');
  }
  const hash = await hashFile(file, { signal });
  if (hash !== session.sha256) throw clientError('client.file_mismatch');
  return hash;
};

export const waitForUpload = async (session, onProgress, control = {}, pollInterval) => {
  const nextDelay = createPollDelay();
  let current = session;
  control.onBackground?.(session);
  control.onSession?.(session);
  for (;;) {
    if (control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
    if (control.signal?.aborted) throw new DOMException('Upload canceled', 'AbortError');
    if (current.status === 'completed') {
      onProgress?.(100);
      return { file: { id: current.completed_entry_id, name: current.filename }, session: current };
    }
    if (['failed', 'aborted', 'expired'].includes(current.status)) {
      const error = clientError(current.error_code || 'upload.conflict', '上传校验失败，请重新上传');
      error.uploadStatus = current.status;
      error.session = current;
      throw error;
    }
    onProgress?.(99);
    await sleep(pollInterval ?? control.pollInterval ?? nextDelay(current.status, current.poll_after_ms), control.signal);
    if (control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
    current = await uploadRequest(`/api/v1/uploads/${session.id}`, {}, control, 'poll');
    control.onSession?.(current);
  }
};

export const uploadFile = async (file, folderId, onProgress, control = {}, existingSession = null) => {
  if (existingSession?.status === 'completed') {
    onProgress?.(100);
    return { file: { id: existingSession.completed_entry_id, name: existingSession.filename }, session: existingSession };
  }
  if (['completing', 'verifying'].includes(existingSession?.status)) return waitForUpload(existingSession, onProgress, control);

  let session = existingSession ? await getUpload(existingSession.id, control) : null;
  if (session && ['completing', 'verifying', 'completed'].includes(session.status)) return waitForUpload(session, onProgress, control);
  if (session && ['failed', 'aborted', 'expired'].includes(session.status)) return waitForUpload(session, onProgress, control);
  const releaseHash = await uploadScheduler.hash(control.signal);
  let hash;
  try {
    hash = session ? await validateResumeFile(file, session, control.signal) : await hashFile(file, { signal: control.signal });
  } finally { releaseHash(); }
  if (control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
  if (control.isPaused?.()) return { paused: true, session };
  if (!session) {
    const input = { folder_id: folderId, filename: file.name, sha256: hash, size_bytes: file.size, mime_type: file.type || 'application/octet-stream' };
    control.creation = input;
    const created = await uploadRequest('/api/v1/uploads', {
      method: 'POST', headers: { 'Idempotency-Key': control.idempotencyKey || `upload-${randomId()}` },
      body: JSON.stringify(input),
    }, control);
    if (created.mode === 'instant') return { instant: true, file: created.file };
    session = created.upload;
  }
  control.onSession?.(session);
  const initializationDelay = createPollDelay();
  while (session.status === 'initializing') {
    if (control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
    if (control.isPaused?.()) return { paused: true, session };
    await sleep(control.pollInterval ?? initializationDelay(session.status, session.poll_after_ms), control.signal);
    if (control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
    if (control.isPaused?.()) return { paused: true, session };
    session = await getUpload(session.id, control);
    control.onSession?.(session);
  }
  if (session.status === 'completed') {
    onProgress?.(100);
    return { file: { id: session.completed_entry_id, name: session.filename }, session };
  }
  if (['completing', 'verifying'].includes(session.status)) return waitForUpload(session, onProgress, control);
  if (!['created', 'uploading'].includes(session.status)) throw clientError(session.error_code || 'upload.conflict');

  const result = await multipartUpload(file, session, onProgress, control);
  if (result.paused) return result;
  session = result.session;
  if (control.isPaused?.()) return { paused: true, session };
  if (control.signal?.aborted || control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
  try {
    session = await uploadRequest(`/api/v1/uploads/${session.id}/complete`, { method: 'POST' }, control);
  } catch (error) {
    if (control.signal?.aborted) throw error;
    const current = await getUpload(session.id, control);
    if (!['completing', 'verifying', 'completed'].includes(current.status)) throw error;
    session = current;
  }
  control.onSession?.(session);
  return waitForUpload(session, onProgress, control);
};
