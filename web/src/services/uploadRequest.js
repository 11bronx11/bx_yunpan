import { api, clientError } from './api';
import { sleep } from './polling';
import { uploadAborted, uploadPolicy } from './uploadPolicy';
import { uploadScheduler } from './uploadScheduler';

export const retryableUploadError = error => error.name !== 'AbortError' &&
  (!error.status || [408, 429, 500, 502, 503, 504].includes(error.status)) &&
  !['upload.part_conflict', 'client.upload_response'].includes(error.code);

export const retryUpload = async (operation, signal, policy = uploadPolicy) => {
  for (let attempt = 0; ; attempt += 1) {
    if (signal?.aborted) throw uploadAborted();
    try { return await operation(attempt); } catch (error) {
      if (signal?.aborted) throw uploadAborted();
      if (attempt + 1 >= policy.attempts || !retryableUploadError(error)) throw error;
      await sleep(Math.max(error.retryAfter || 0, Math.min(500 * 2 ** attempt, 4000) * (0.8 + Math.random() * 0.4)), signal);
    }
  }
};

export const uploadRequest = (path, options = {}, control = {}, kind = 'control') => {
  const policy = { ...uploadPolicy, ...control.policy };
  return retryUpload(async () => {
    const priority = options.method === 'DELETE' ? 2 : path.includes('/parts/confirm') ? 1 : 0;
    const release = await uploadScheduler[kind](control.signal, priority);
    const controller = new AbortController();
    const abort = () => controller.abort();
    control.signal?.addEventListener('abort', abort, { once: true });
    if (control.signal?.aborted) controller.abort();
    const timer = setTimeout(abort, policy.requestTimeout);
    try { return await api(path, { ...options, signal: controller.signal }); } catch (error) {
      if (controller.signal.aborted && !control.signal?.aborted) throw clientError('client.upload_timeout');
      throw error;
    } finally {
      clearTimeout(timer);
      control.signal?.removeEventListener('abort', abort);
      release();
    }
  }, control.signal, policy);
};
