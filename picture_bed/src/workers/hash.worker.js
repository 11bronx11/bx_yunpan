/* eslint-disable no-restricted-globals */
import { createSHA256 } from 'hash-wasm';

const chunkSize = 8 * 1024 * 1024;

self.onmessage = async event => {
  try {
    const file = event.data;
    const hasher = await createSHA256();
    hasher.init();
    for (let offset = 0; offset < file.size; offset += chunkSize) {
      const bytes = await file.slice(offset, Math.min(offset + chunkSize, file.size)).arrayBuffer();
      hasher.update(new Uint8Array(bytes));
    }
    self.postMessage({ hash: hasher.digest() });
  } catch {
    self.postMessage({ error: '文件校验失败' });
  }
};
