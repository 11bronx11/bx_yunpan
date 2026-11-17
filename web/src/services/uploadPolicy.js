export const uploadPolicy = Object.freeze({
  files: 4, parts: 4, globalParts: 8, hash: 2,
  presign: 20, confirm: 8, confirmWait: 250,
  pending: 16, globalPending: 32, control: 4, polls: 4, background: 16,
  attempts: 4, requestTimeout: 15000, partTimeout: 120000,
});

export const uploadAborted = () => new DOMException('Upload canceled', 'AbortError');
