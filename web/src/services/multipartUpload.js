import { clientError } from './api';
import { uploadAborted, uploadPolicy } from './uploadPolicy';
import { createUploadGate, releasePendingParts, uploadScheduler } from './uploadScheduler';
import { uploadPart } from './uploadPart';
import { retryUpload, uploadRequest } from './uploadRequest';

// Successful PUTs retain their ETags in memory until the database acknowledges them.
export const multipartUpload = async (file, initialSession, onProgress, control) => {
  const policy = { ...uploadPolicy, ...control.policy };
  const controller = new AbortController();
  const abort = () => controller.abort();
  control.signal?.addEventListener('abort', abort, { once: true });
  if (control.signal?.aborted) controller.abort();
  const signal = controller.signal;
  const requestControl = { ...control, signal };
  let session = initialSession;
  const confirmed = new Map((session.confirmed_parts || []).map(p => [p.part_number, p]));
  const retained = control.pendingParts || new Map();
  control.pendingParts = retained;
  for (const number of confirmed.keys()) { retained.get(number)?.releasePending?.(); retained.delete(number); }
  const pending = new Map();
  const pendingSince = new Map();
  const permits = new Map();
  const inFlight = new Map();
  const localPending = createUploadGate(policy.pending);
  const signatures = new Map();
  let signing, confirming, timer, failure, draining = false, cursor = 0;
  let lastEmit = 0, wireBytes = 0;
  const missing = Array.from({ length: session.part_count }, (_, i) => i + 1).filter(n => !confirmed.has(n) && !retained.has(n));
  const sizeOf = number => Math.min(session.part_size, file.size - (number - 1) * session.part_size);
  const emit = (force = false) => {
    if (!force && Date.now() - lastEmit < 100) return;
    lastEmit = Date.now();
    const confirmedBytes = [...confirmed.values()].reduce((sum, p) => sum + p.size_bytes, 0);
    const uploadedBytes = confirmedBytes + [...retained.values()].reduce((sum, p) => sum + p.size_bytes, 0);
    const transferred = Math.min(file.size, uploadedBytes + [...inFlight.values()].reduce((a, b) => a + b, 0));
    control.onBytes?.({ confirmedBytes, uploadedBytes, transferred, wireBytes });
    onProgress?.(Math.min(99, Math.floor(transferred / file.size * 100)));
  };
  const publishSession = () => {
    session = { ...session, confirmed_parts: [...confirmed.values()].sort((a, b) => a.part_number - b.part_number) };
    control.onSession?.(session);
    emit(true);
  };
  const fail = error => { if (!failure) failure = error; controller.abort(); };
  const reserve = async number => {
    const local = await localPending(signal);
    try {
      const global = retained.get(number)?.releasePending || await uploadScheduler.pending(signal);
      permits.set(number, { global, local, release: () => { global(); local(); } });
    } catch (error) { local(); throw error; }
  };
  const scheduleConfirm = (force = false) => {
    if (confirming || failure || !pending.size) return;
    if (!force && !draining && pending.size < policy.confirm) {
      if (!timer) {
        const oldest = Math.min(...pendingSince.values());
        timer = setTimeout(() => { timer = null; scheduleConfirm(true); }, Math.max(0, oldest + policy.confirmWait - Date.now()));
      }
      return;
    }
    clearTimeout(timer); timer = null;
    const batch = [...pending.values()].slice(0, Math.min(policy.confirm, 100));
    for (const p of batch) { pending.delete(p.part_number); pendingSince.delete(p.part_number); }
    confirming = (async () => {
      const response = await uploadRequest(`/api/v1/uploads/${session.id}/parts/confirm?view=delta`, {
        method: 'POST', body: JSON.stringify({ parts: batch }),
      }, requestControl);
      const ack = response.acknowledged_parts || response.confirmed_parts;
      if ((response.upload_id || response.id) !== session.id || !Array.isArray(ack) ||
          !batch.every(p => ack.some(a => a.part_number === p.part_number && a.size_bytes === p.size_bytes))) {
        throw clientError('client.upload_response');
      }
      for (const p of batch) {
        confirmed.set(p.part_number, { part_number: p.part_number, size_bytes: p.size_bytes });
        retained.delete(p.part_number);
        permits.get(p.part_number)?.release(); permits.delete(p.part_number);
      }
      session = { ...session, status: response.status };
      publishSession();
    })().catch(fail).finally(() => { confirming = null; scheduleConfirm(); });
  };
  const signature = async number => {
    const current = signatures.get(number);
    if (current && (!current.expires_at || Date.parse(current.expires_at) - Date.now() > 30000)) return current;
    signatures.delete(number);
    if (signing) { await signing; return signature(number); }
    if (!signing) {
      const numbers = missing.filter(n => n >= number && !confirmed.has(n) && !retained.has(n) && !signatures.has(n)).slice(0, Math.min(policy.presign, 20));
      if (!numbers.includes(number)) numbers.unshift(number);
      signing = uploadRequest(`/api/v1/uploads/${session.id}/parts/presign`, {
        method: 'POST', body: JSON.stringify({ part_numbers: numbers.slice(0, 20) }),
      }, requestControl).then(response => {
        for (const p of response.parts || []) signatures.set(p.part_number, p);
      }).finally(() => { signing = null; });
    }
    await signing;
    const value = signatures.get(number);
    if (!value?.url) throw clientError('client.upload_response');
    return value;
  };
  const sendNumber = async number => {
    await reserve(number);
    if (control.isPaused?.()) { permits.get(number)?.release(); permits.delete(number); return; }
    const body = file.slice((number - 1) * session.part_size, (number - 1) * session.part_size + sizeOf(number));
    const etag = await retryUpload(async attempt => {
      if (attempt) {
        // An uncertain PUT is only repeated after checking durable confirmation.
        const current = await uploadRequest(`/api/v1/uploads/${session.id}`, {}, requestControl);
        if ((current.confirmed_parts || []).some(p => p.part_number === number)) {
          confirmed.set(number, current.confirmed_parts.find(p => p.part_number === number));
          return null;
        }
      }
      const signed = await signature(number);
      const release = await uploadScheduler.put(signal);
      let sent = 0;
      try {
        return await uploadPart(signed.url, body, { signal, timeout: policy.partTimeout, onProgress: bytes => {
          wireBytes += Math.max(0, bytes - sent); sent = bytes;
          inFlight.set(number, bytes); emit();
        } });
      } catch (error) {
        if (error.expired) signatures.delete(number);
        throw error;
      } finally { inFlight.delete(number); release(); }
    }, signal, policy);
    signatures.delete(number);
    if (etag !== null) {
      const part = { part_number: number, etag, size_bytes: body.size };
      Object.defineProperty(part, 'releasePending', { value: permits.get(number).global });
      retained.set(number, part); pending.set(number, part); pendingSince.set(number, Date.now());
      scheduleConfirm();
    } else { permits.get(number)?.release(); permits.delete(number); publishSession(); }
    emit(true);
  };
  const worker = async () => {
    while (cursor < missing.length && !control.isPaused?.() && !signal.aborted) await sendNumber(missing[cursor++]);
  };
  try {
    // Replay retained ETags before sending missing bytes after a same-page retry.
    for (const part of [...retained.values()]) {
      await reserve(part.part_number);
      pending.set(part.part_number, part); pendingSince.set(part.part_number, Date.now());
      scheduleConfirm();
    }
    emit(true);
    await Promise.all(Array.from({ length: policy.parts }, () => worker().catch(fail)));
    draining = true;
    scheduleConfirm(true);
    while (confirming) await confirming;
    if (failure) throw failure;
    if (signal.aborted || control.isCanceled?.()) throw uploadAborted();
    publishSession();
    if (control.isPaused?.()) return { paused: true, session };
    if (confirmed.size !== session.part_count || retained.size) throw clientError('client.upload_response');
    return { session };
  } finally {
    controller.abort();
    clearTimeout(timer);
    if (confirming) await confirming;
    for (const [number, lease] of permits) {
      lease.local();
      if (!retained.has(number)) lease.global();
    }
    if (control.signal?.aborted || control.isCanceled?.()) releasePendingParts(retained);
    control.signal?.removeEventListener('abort', abort);
  }
};
