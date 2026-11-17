import { clientError } from './api';
import { uploadAborted } from './uploadPolicy';

export const hashFile = (file, { signal } = {}) => new Promise((resolve, reject) => {
  if (signal?.aborted) { reject(uploadAborted()); return; }
  const worker = new Worker(new URL('../workers/hash.worker.js', import.meta.url));
  const finish = (error, hash) => {
    worker.terminate();
    signal?.removeEventListener('abort', abort);
    if (error) reject(error); else resolve(hash);
  };
  const abort = () => finish(uploadAborted());
  signal?.addEventListener('abort', abort, { once: true });
  worker.onmessage = event => finish(event.data.error ? clientError('client.hash_failed') : null, event.data.hash);
  worker.onerror = () => finish(clientError('client.hash_failed'));
  worker.postMessage(file);
});
