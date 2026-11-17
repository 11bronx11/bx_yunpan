import { clientError } from './api';
import { uploadAborted } from './uploadPolicy';

export const uploadPart = (url, body, { signal, timeout, onProgress }) => new Promise((resolve, reject) => {
  if (signal?.aborted) { reject(uploadAborted()); return; }
  const xhr = new XMLHttpRequest();
  const abort = () => xhr.abort();
  const finish = (error, value) => {
    signal?.removeEventListener('abort', abort);
    if (error) reject(error); else resolve(value);
  };
  xhr.open('PUT', url);
  xhr.timeout = timeout;
  xhr.upload.onprogress = event => onProgress?.(Math.min(event.loaded, body.size));
  xhr.onload = () => {
    if (xhr.status < 200 || xhr.status >= 300) {
      const error = clientError('client.upload_part');
      error.status = xhr.status;
      error.expired = xhr.status === 403 && /RequestExpired|ExpiredToken|RequestTimeTooSkewed/.test(xhr.responseText);
      if (error.expired) error.status = 408;
      const retryAfter = xhr.getResponseHeader('Retry-After');
      error.retryAfter = /^\d+$/.test(retryAfter || '') ? Number(retryAfter) * 1000 : Math.max(0, Date.parse(retryAfter) - Date.now()) || 0;
      finish(error);
      return;
    }
    const etag = (xhr.getResponseHeader('ETag') || '').trim().replace(/^"|"$/g, '');
    if (!etag) { finish(clientError('client.upload_etag')); return; }
    onProgress?.(body.size);
    finish(null, etag);
  };
  xhr.onerror = () => finish(clientError('client.network'));
  xhr.ontimeout = () => finish(clientError('client.upload_timeout'));
  xhr.onabort = () => finish(uploadAborted());
  signal?.addEventListener('abort', abort, { once: true });
  xhr.send(body);
});
