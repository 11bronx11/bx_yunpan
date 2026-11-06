export const sleep = (milliseconds, signal) => new Promise((resolve, reject) => {
  const aborted = () => new DOMException('Operation canceled', 'AbortError');
  if (signal?.aborted) {
    reject(aborted());
    return;
  }
  const onAbort = () => {
    clearTimeout(timer);
    reject(aborted());
  };
  const timer = setTimeout(() => {
    signal?.removeEventListener('abort', onAbort);
    resolve();
  }, milliseconds);
  signal?.addEventListener('abort', onAbort, { once: true });
});

// Back off only while the observed state is unchanged; progress resets the delay.
export const createPollDelay = ({ initial = 1000, max = 5000 } = {}) => {
  let previous;
  let delay = initial;
  return (state, suggested) => {
    const minimum = Number.isFinite(suggested) && suggested > 0 ? Math.min(suggested, max) : initial;
    delay = state === previous ? Math.min(Math.max(delay * 2, minimum), max) : minimum;
    previous = state;
    return delay;
  };
};
