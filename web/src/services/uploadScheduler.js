import { uploadAborted, uploadPolicy } from './uploadPolicy';

// FIFO waiters prevent a fast file from overtaking already queued files.
export const createUploadGate = limit => {
  let used = 0;
  const queue = [];
  const grant = entry => {
    entry.signal?.removeEventListener('abort', entry.abort);
    used += 1;
    let released = false;
    entry.resolve(() => {
      if (released) return;
      released = true;
      used -= 1;
      drain();
    });
  };
  const drain = () => {
    while (used < limit && queue.length) grant(queue.shift());
  };
  return (signal, priority = 0) => new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(uploadAborted()); return; }
    const entry = { resolve, signal, priority, abort: () => {
      const index = queue.indexOf(entry);
      if (index >= 0) queue.splice(index, 1);
      reject(uploadAborted());
    } };
    signal?.addEventListener('abort', entry.abort, { once: true });
    const index = queue.findIndex(waiter => waiter.priority < priority);
    if (index < 0) queue.push(entry); else queue.splice(index, 0, entry);
    drain();
  });
};

export const uploadScheduler = {
  put: createUploadGate(uploadPolicy.globalParts),
  pending: createUploadGate(uploadPolicy.globalPending),
  hash: createUploadGate(uploadPolicy.hash),
  control: createUploadGate(uploadPolicy.control),
  poll: createUploadGate(uploadPolicy.polls),
};

export const releasePendingParts = parts => {
  parts?.forEach(part => part.releasePending?.());
  parts?.clear();
};
