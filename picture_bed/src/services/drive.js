import { api, clientError } from './api';
import { hashFile } from './fileHash';

export const getRoot = () => api('/api/v1/folders/root');
export const getChildren = folderId => api(`/api/v1/folders/${folderId}/children`);
export const getBreadcrumb = folderId => api(`/api/v1/folders/${folderId}/breadcrumb`);
export const createFolder = (parentId, name) => api('/api/v1/folders', { method: 'POST', body: JSON.stringify({ parent_id: parentId, name }) });
export const renameFolder = (id, version, name) => api(`/api/v1/folders/${id}`, { method: 'PATCH', headers: { 'If-Match': String(version) }, body: JSON.stringify({ name }) });
export const deleteFolder = (id, version) => api(`/api/v1/folders/${id}`, { method: 'DELETE', headers: { 'If-Match': String(version) } });
export const renameFile = (id, version, name) => api(`/api/v1/files/${id}`, { method: 'PATCH', headers: { 'If-Match': String(version) }, body: JSON.stringify({ name }) });
export const moveFile = (id, version, targetFolderId) => api(`/api/v1/files/${id}/move`, { method: 'POST', headers: { 'If-Match': String(version) }, body: JSON.stringify({ target_folder_id: targetFolderId }) });
export const deleteFile = (id, version) => api(`/api/v1/files/${id}`, { method: 'DELETE', headers: { 'If-Match': String(version) } });
export const getDownloadURL = id => api(`/api/v1/files/${id}/download-url`);
export const getPreview = id => api(`/api/v1/files/${id}/preview`);

const sleep = (milliseconds, signal) => new Promise((resolve, reject) => {
  if (signal?.aborted) {
    reject(new DOMException('Upload canceled', 'AbortError'));
    return;
  }
  let timer;
  const onAbort = () => {
    clearTimeout(timer);
    reject(new DOMException('Upload canceled', 'AbortError'));
  };
  timer = setTimeout(() => {
    signal?.removeEventListener('abort', onAbort);
    resolve();
  }, milliseconds);
  signal?.addEventListener('abort', onAbort, { once: true });
});
const randomId = () => window.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(16).slice(2)}`;

export const listActiveUploads = () => api('/api/v1/uploads?status=active');
export const abortUpload = uploadId => api(`/api/v1/uploads/${uploadId}`, { method: 'DELETE' });

const validateResumeFile = async (file, session) => {
  if (file.name !== session.filename || file.size !== session.size_bytes) {
    throw clientError('client.file_mismatch');
  }
  const hash = await hashFile(file);
  if (hash !== session.sha256) throw clientError('client.file_mismatch');
  return hash;
};

export const waitForUpload = async (session, onProgress, control = {}, pollInterval = 1000) => {
  for (;;) {
    if (control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
    const current = await api(`/api/v1/uploads/${session.id}`, { signal: control.signal });
    control.onSession?.(current);
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
    onProgress?.(95);
    await sleep(pollInterval, control.signal);
  }
};

export const uploadFile = async (file, folderId, onProgress, control = {}, existingSession = null) => {
  if (existingSession?.status === 'completed') {
    onProgress?.(100);
    return { file: { id: existingSession.completed_entry_id, name: existingSession.filename }, session: existingSession };
  }
  if (existingSession?.status === 'verifying') return waitForUpload(existingSession, onProgress, control);

  const hash = existingSession ? await validateResumeFile(file, existingSession) : await hashFile(file);
  if (control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
  let session = existingSession;
  if (!session) {
    const created = await api('/api/v1/uploads', {
      method: 'POST', headers: { 'Idempotency-Key': control.idempotencyKey || `upload-${randomId()}` },
      body: JSON.stringify({ folder_id: folderId, filename: file.name, sha256: hash, size_bytes: file.size, mime_type: file.type || 'application/octet-stream' }),
    });
    if (created.mode === 'instant') return { instant: true, file: created.file };
    session = created.upload;
  }
  control.onSession?.(session);
  if (session.status === 'completed') {
    onProgress?.(100);
    return { file: { id: session.completed_entry_id, name: session.filename }, session };
  }
  if (session.status === 'verifying') return waitForUpload(session, onProgress, control);

  const partSize = session.part_size;
  const confirmed = new Set((session.confirmed_parts || []).map(part => part.part_number));
  let confirmedBytes = (session.confirmed_parts || []).reduce((total, part) => total + part.size_bytes, 0);
  onProgress?.(Math.round(Math.min(confirmedBytes / file.size, 1) * 90));
  for (let index = 0; index < session.part_count; index += 1) {
    if (control.isCanceled?.()) throw new DOMException('Upload canceled', 'AbortError');
    if (control.isPaused?.()) return { paused: true, session };
    const partNumber = index + 1;
    if (confirmed.has(partNumber)) continue;
    const presigned = await api(`/api/v1/uploads/${session.id}/parts/presign`, { method: 'POST', body: JSON.stringify({ part_numbers: [partNumber] }) });
    const part = presigned.parts[0];
    const body = file.slice(index * partSize, Math.min((index + 1) * partSize, file.size));
    let response;
    try {
      response = await fetch(part.url, { method: 'PUT', body, signal: control.signal });
    } catch (error) {
      if (error.name === 'AbortError') throw error;
      throw clientError('client.network');
    }
    if (!response.ok) throw new Error(`分片 ${partNumber} 上传失败`);
    const etag = (response.headers.get('ETag') || response.headers.get('etag') || `part-${partNumber}`).replaceAll('"', '');
    session = await api(`/api/v1/uploads/${session.id}/parts/confirm`, { method: 'POST', body: JSON.stringify({ parts: [{ part_number: partNumber, etag, size_bytes: body.size }] }) });
    confirmed.add(partNumber);
    confirmedBytes += body.size;
    control.onSession?.(session);
    onProgress?.(Math.round(Math.min(confirmedBytes / file.size, 1) * 90));
  }
  session = await api(`/api/v1/uploads/${session.id}/complete`, { method: 'POST' });
  control.onSession?.(session);
  return waitForUpload(session, onProgress, control);
};
