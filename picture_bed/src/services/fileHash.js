import { clientError } from './api';

export const hashFile = file => new Promise((resolve, reject) => {
  const worker = new Worker(new URL('../workers/hash.worker.js', import.meta.url));
  worker.onmessage = event => {
    worker.terminate();
    if (event.data.error) reject(clientError('client.hash_failed'));
    else resolve(event.data.hash);
  };
  worker.onerror = () => {
    worker.terminate();
    reject(clientError('client.hash_failed'));
  };
  worker.postMessage(file);
});
