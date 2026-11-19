import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import { abortUpload, getUpload, listActiveUploads, uploadFile, waitForUpload } from '../services/drive';
import { releasePendingParts } from '../services/uploadScheduler';
import { uploadPolicy } from '../services/uploadPolicy';
import { uploadRequest } from '../services/uploadRequest';
import { createUploadSpeedMeter } from '../services/uploadSpeed';
import { useAuth } from './AuthContext';

const TransferContext = createContext(null);

export const MAX_BATCH_FILES = 10;
export const MAX_CONCURRENT_UPLOADS = uploadPolicy.files;

const terminalStatuses = new Set(['completed', 'skipped', 'failed', 'canceled']);
const persistedStatuses = new Set(['queued', 'checking', 'uploading', 'pausing', 'paused', 'completing', 'canceling', 'cancel_pending', 'verifying', 'verification_pending', 'interrupted']);
const activeStatuses = new Set([...persistedStatuses]);
const storageKey = userId => `bx-yunpan:uploads:${userId}`;
const randomId = () => window.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(16).slice(2)}`;

const readStoredUploads = userId => {
  try {
    const value = JSON.parse(localStorage.getItem(storageKey(userId)) || '[]');
    return Array.isArray(value) ? value : [];
  } catch {
    return [];
  }
};

const storedUpload = task => ({
  id: task.id,
  folderId: task.folderId,
  destination: task.destination,
  name: task.name,
  size: task.size,
  mimeType: task.mimeType,
  progress: task.progress,
  transferred: task.transferred,
  session: task.session,
  idempotencyKey: task.idempotencyKey,
  createdAt: task.createdAt,
  cancelRequested: task.cancelRequested,
  creation: task.creation || task.control?.creation,
});

const sessionProgress = session => {
  if (session.status === 'completed') return 100;
  if (['completing', 'verifying'].includes(session.status)) return 99;
  const bytes = (session.confirmed_parts || []).reduce((total, part) => total + part.size_bytes, 0);
  return session.size_bytes > 0 ? Math.min(99, Math.floor(bytes / session.size_bytes * 100)) : 0;
};

const recoveredUpload = (session, local) => {
  const progress = sessionProgress(session);
  const terminal = ['completed', 'failed', 'aborted', 'expired'].includes(session.status);
  return {
    id: local?.id || `upload-${session.id}`,
    file: null,
    folderId: session.folder_id,
    destination: local?.destination || '目标目录',
    name: session.filename,
    size: session.size_bytes,
    mimeType: local?.mimeType || 'application/octet-stream',
    status: terminal ? session.status === 'completed' ? 'completed' : session.status === 'failed' ? 'failed' : 'canceled'
      : local?.cancelRequested ? 'cancel_pending' : ['completing', 'verifying'].includes(session.status) ? session.status : 'interrupted',
    cancelRequested: !terminal && local?.cancelRequested,
    progress,
    transferred: ['completed', 'completing', 'verifying'].includes(session.status) ? session.size_bytes : (session.confirmed_parts || []).reduce((sum, p) => sum + p.size_bytes, 0),
    completedFile: session.status === 'completed' ? { id: session.completed_entry_id, name: session.filename } : null,
    speed: 0,
    session,
    idempotencyKey: local?.idempotencyKey,
    error: null,
    createdAt: local?.createdAt || Date.parse(session.created_at) || Date.now(),
  };
};

const localInterruptedUpload = local => ({
  ...local,
  file: null,
  status: local.cancelRequested ? 'cancel_pending' : ['completing', 'verifying'].includes(local.session?.status) ? local.session.status : 'interrupted',
  progress: local.progress || 0,
  transferred: local.transferred || 0,
  speed: 0,
  control: null,
  error: null,
});

export const TransferProvider = ({ children }) => {
  const { user, ready } = useAuth();
  const [uploads, setUploads] = useState([]);
  const uploadsRef = useRef(uploads);
  const activeUploads = useRef(new Set());
  const transferSlots = useRef(new Set());
  const controls = useRef(new Map());
  const cancelControls = useRef(new Map());
  const ownerId = useRef(null);
  const hydratedOwner = useRef(null);

  useEffect(() => { uploadsRef.current = uploads; }, [uploads]);

  useEffect(() => () => {
    controls.current.forEach(control => { control.canceled = true; control.abortController.abort(); });
    cancelControls.current.forEach(controller => controller.abort());
    uploadsRef.current.forEach(task => {
      releasePendingParts(task.pendingParts);
      if (task.control) {
        task.control.canceled = true;
        task.control.abortController.abort();
      }
    });
    activeUploads.current.clear();
  }, []);

  useEffect(() => {
    if (!ready) return undefined;
    const nextOwnerId = user?.id || null;
    if (ownerId.current !== nextOwnerId) {
      controls.current.forEach(control => { control.canceled = true; control.abortController.abort(); });
      cancelControls.current.forEach(controller => controller.abort());
      controls.current.clear();
      cancelControls.current.clear();
      transferSlots.current.clear();
      uploadsRef.current.forEach(task => {
        releasePendingParts(task.pendingParts);
        if (task.control) {
          task.control.canceled = true;
          task.control.abortController.abort();
        }
      });
      activeUploads.current.clear();
      uploadsRef.current = [];
      setUploads([]);
      ownerId.current = nextOwnerId;
      hydratedOwner.current = null;
    }
    if (!nextOwnerId) return undefined;

    let mounted = true;
    const controller = new AbortController();
    const local = readStoredUploads(nextOwnerId);
    const localBySession = new Map(local.filter(task => task.session?.id).map(task => [task.session.id, task]));
    const restoreUploads = recovered => setUploads(current => {
      const ids = new Set(current.map(task => task.id));
      const sessions = new Set(current.map(task => task.session?.id).filter(Boolean));
      return [...current, ...recovered.filter(task => !ids.has(task.id) && !sessions.has(task.session?.id))];
    });
    listActiveUploads({ signal: controller.signal })
      .then(async response => {
        if (!mounted || ownerId.current !== nextOwnerId) return;
        const sessions = response.items || [];
        const backendTasks = sessions.map(session => recoveredUpload(session, localBySession.get(session.id)));
        const activeSessionIds = new Set(sessions.map(session => session.id));
        // Terminal sessions disappear from the active list; reconcile their local evidence separately.
        const localOnly = await Promise.all(local.filter(task => !activeSessionIds.has(task.session?.id)).map(async task => {
          if (!task.session?.id) return localInterruptedUpload(task);
          try {
            return recoveredUpload(await getUpload(task.session.id, { signal: controller.signal }), task);
          } catch {
            return localInterruptedUpload(task);
          }
        }));
        if (!mounted || ownerId.current !== nextOwnerId) return;
        hydratedOwner.current = nextOwnerId;
        restoreUploads([...backendTasks, ...localOnly]);
      })
      .catch(() => {
        if (!mounted || ownerId.current !== nextOwnerId) return;
        hydratedOwner.current = nextOwnerId;
        restoreUploads(local.map(localInterruptedUpload));
      });
    return () => { mounted = false; controller.abort(); };
  }, [ready, user?.id]);

  useEffect(() => {
    if (!ready || !user?.id || hydratedOwner.current !== user.id) return;
    const persist = () => {
      const records = uploadsRef.current.filter(task => persistedStatuses.has(task.status) || (task.status === 'failed' && (task.session || task.creation || task.control?.creation))).map(storedUpload);
      try {
        if (records.length > 0) localStorage.setItem(storageKey(user.id), JSON.stringify(records));
        else localStorage.removeItem(storageKey(user.id));
      } catch { /* In-memory transfers still work when storage is unavailable. */ }
    };
    const timer = setTimeout(persist, 300);
    window.addEventListener('pagehide', persist);
    return () => { clearTimeout(timer); window.removeEventListener('pagehide', persist); };
  }, [ready, uploads, user?.id]);

  useEffect(() => {
    const hasBrowserUpload = uploads.some(task => task.file && ['queued', 'checking', 'uploading', 'pausing', 'canceling'].includes(task.status));
    if (!hasBrowserUpload) return undefined;
    const warnBeforeUnload = event => {
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', warnBeforeUnload);
    return () => window.removeEventListener('beforeunload', warnBeforeUnload);
  }, [uploads]);

  const updateUpload = useCallback((id, change) => {
    const next = uploadsRef.current.map(task => task.id === id ? { ...task, ...(typeof change === 'function' ? change(task) : change) } : task);
    uploadsRef.current = next;
    setUploads(next);
  }, []);

  const runUpload = useCallback(async id => {
    if (activeUploads.current.has(id)) return;
    const task = uploadsRef.current.find(item => item.id === id);
    const background = ['completing', 'verifying'].includes(task?.status);
    if (!task || (!background && task.status !== 'queued')) return;
    if (!background && !task.file) { updateUpload(id, { status: 'interrupted' }); return; }
    activeUploads.current.add(id);
    if (!background) transferSlots.current.add(id);
    const runOwner = ownerId.current;
    const abortController = new AbortController();
    let finished;
    const control = {
      paused: false, canceled: false, signal: abortController.signal, abortController,
      idempotencyKey: task.idempotencyKey, pendingParts: task.pendingParts || new Map(),
      session: task.session, creation: task.creation,
      done: new Promise(resolve => { finished = resolve; }),
      isPaused: () => control.paused, isCanceled: () => control.canceled,
    };
    controls.current.set(id, control);
    const speedMeter = createUploadSpeedMeter();
    let speedTimer;
    const stopSpeed = () => { window.clearInterval(speedTimer); speedTimer = undefined; };
    abortController.signal.addEventListener('abort', stopSpeed, { once: true });
    const current = () => ownerId.current === runOwner && controls.current.get(id) === control && !control.canceled;
    control.onSession = session => {
      control.session = session;
      if (!current()) return;
      updateUpload(id, item => ({ session, creation: control.creation,
        status: ['completing', 'verifying'].includes(session.status) ? session.status : item.status === 'checking' ? 'uploading' : item.status,
      }));
    };
    control.onBackground = () => {
      if (!current()) return;
      stopSpeed();
      transferSlots.current.delete(id);
      updateUpload(id, { speed: 0 });
    };
    control.onBytes = bytes => {
      if (!current()) return;
      speedMeter.update(bytes.wireBytes);
      if (speedTimer === undefined && !control.paused) {
        speedTimer = window.setInterval(() => {
          if (!current()) return;
          const speed = speedMeter.read();
          updateUpload(id, item => ({ speed: item.status === 'uploading' ? speed : 0 }));
        }, 500);
      }
      updateUpload(id, bytes);
    };
    const onProgress = progress => { if (current()) updateUpload(id, { progress }); };
    updateUpload(id, { status: background ? task.status : 'checking', control, pendingParts: control.pendingParts,
      error: null, startedAt: Date.now(), speed: 0 });
    try {
      const result = background ? await waitForUpload(task.session, onProgress, control)
        : await uploadFile(task.file, task.folderId, onProgress, control, task.session);
      control.result = result;
      if (!current()) return;
      if (result.paused) { updateUpload(id, { status: 'paused', session: result.session, control: null, speed: 0 }); return; }
      updateUpload(id, { status: 'completed', progress: 100, transferred: task.size, speed: 0, control: null,
        session: result.session || control.session, instant: Boolean(result.instant), completedFile: result.file,
        completedAt: Date.now(), pendingParts: null, creation: null });
    } catch (error) {
      if (!current()) return;
      if (['upload.file_exists', 'upload.name_conflict'].includes(error.code)) {
        updateUpload(id, { status: 'skipped', progress: error.code === 'upload.file_exists' ? 100 : 0,
          speed: 0, control: null, error: error.message, errorCode: error.code });
      } else {
        const terminal = ['failed', 'aborted', 'expired'].includes(error.uploadStatus);
        if (terminal) releasePendingParts(control.pendingParts);
        updateUpload(id, { status: error.code === 'client.file_mismatch' ? 'interrupted' : 'failed',
          error: error.message || '上传失败，请重试', control: null, speed: 0, creation: control.creation,
          ...(error.code === 'client.file_mismatch' ? { file: null } : {}),
          ...(terminal ? { session: null, pendingParts: null, creation: null, idempotencyKey: `upload-${randomId()}`, progress: 0, transferred: 0 } : {}) });
      }
    } finally {
      stopSpeed();
      abortController.signal.removeEventListener('abort', stopSpeed);
      if (controls.current.get(id) === control) {
        activeUploads.current.delete(id); transferSlots.current.delete(id); controls.current.delete(id);
        if (ownerId.current === runOwner) setUploads([...uploadsRef.current]);
      }
      finished();
    }
  }, [updateUpload]);

  useEffect(() => {
    uploads.filter(task => ['completing', 'verifying'].includes(task.status) && !activeUploads.current.has(task.id)).forEach(task => runUpload(task.id));
    const backgroundCount = uploads.filter(task => ['completing', 'verifying'].includes(task.status)).length;
    if (backgroundCount >= uploadPolicy.background) return;
    const available = Math.min(MAX_CONCURRENT_UPLOADS - transferSlots.current.size, uploadPolicy.background - backgroundCount);
    if (available > 0) uploads.filter(task => task.status === 'queued' && !activeUploads.current.has(task.id)).slice(0, available).forEach(task => runUpload(task.id));
  }, [runUpload, uploads]);

  const enqueueUploads = useCallback((files, folderId, destination) => {
    const selected = Array.from(files || []);
    const accepted = selected.slice(0, MAX_BATCH_FILES);
    const now = Date.now();
    const tasks = accepted.map((file, index) => ({
      id: randomId(),
      file,
      folderId,
      destination,
      name: file.name,
      size: file.size,
      mimeType: file.type || 'application/octet-stream',
      status: 'queued',
      progress: 0,
      transferred: 0,
      speed: 0,
      session: null,
      idempotencyKey: `upload-${randomId()}`,
      error: null,
      createdAt: now + index,
    }));
    setUploads(current => [...tasks, ...current]);
    return { accepted: accepted.length, rejected: selected.length - accepted.length };
  }, []);

  const pauseUpload = useCallback(id => {
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task) return;
    if (task.status === 'queued') {
      updateUpload(id, { status: 'paused', speed: 0 });
      return;
    }
    if (['checking', 'uploading'].includes(task.status) && task.control) {
      task.control.paused = true;
      updateUpload(id, { status: 'pausing', speed: 0 });
    }
  }, [updateUpload]);

  const resumeUpload = useCallback(id => {
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task || !['paused', 'failed', 'verification_pending'].includes(task.status)) return;
    if (['failed', 'aborted', 'expired'].includes(task.session?.status)) {
      releasePendingParts(task.pendingParts);
      updateUpload(id, { status: task.file ? 'queued' : 'interrupted', session: null, creation: null, pendingParts: null,
        idempotencyKey: `upload-${randomId()}`, error: null, control: null, speed: 0, progress: 0, transferred: 0 });
      return;
    }
    const status = ['completing', 'verifying'].includes(task.session?.status) ? task.session.status : 'queued';
    updateUpload(id, { status, error: null, control: null, speed: 0 });
  }, [updateUpload]);

  const selectResumeFile = useCallback((id, file) => {
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task || task.status !== 'interrupted') return false;
    if (file.name !== task.name || file.size !== task.size) {
      updateUpload(id, { error: '所选文件的名称或大小不一致，请选择原文件' });
      return false;
    }
    updateUpload(id, { file, mimeType: file.type || task.mimeType, status: 'queued', error: null, control: null, speed: 0 });
    return false;
  }, [updateUpload]);

  const cancelUpload = useCallback(async id => {
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task || (terminalStatuses.has(task.status) && task.status !== 'failed') || cancelControls.current.has(id)) return;
    const runOwner = ownerId.current;
    const running = controls.current.get(id);
    releasePendingParts(task.pendingParts);
    if (running) { running.canceled = true; running.abortController.abort(); }
    const controller = new AbortController();
    cancelControls.current.set(id, controller);
    const current = () => ownerId.current === runOwner && !controller.signal.aborted;
    updateUpload(id, { status: 'canceling', cancelRequested: true, speed: 0, error: null });
    try {
      if (running) await running.done;
      if (!current()) return;
      if (running?.result?.file) {
        updateUpload(id, { status: 'completed', cancelRequested: false, progress: 100, completedFile: running.result.file, control: null });
        return;
      }
      let session = running?.session || task.session;
      const creation = running?.creation || task.creation;
      if (!session && creation) {
        const created = await uploadRequest('/api/v1/uploads', { method: 'POST', headers: { 'Idempotency-Key': task.idempotencyKey }, body: JSON.stringify(creation) }, { signal: controller.signal });
        if (created.mode === 'instant') {
          if (current()) updateUpload(id, { status: 'completed', cancelRequested: false, progress: 100, completedFile: created.file, control: null });
          return;
        }
        session = created.upload;
      }
      if (!current()) return;
      if (session) {
        updateUpload(id, { session, creation });
        try { await abortUpload(session.id, { signal: controller.signal }); }
        catch (error) {
          const latest = await getUpload(session.id, { signal: controller.signal });
          if (['completing', 'verifying', 'completed'].includes(latest.status)) {
            if (current()) updateUpload(id, { status: latest.status === 'completed' ? 'completed' : latest.status, session: latest,
              cancelRequested: false, control: null, progress: latest.status === 'completed' ? 100 : 99 });
            return;
          }
          if (!['aborted', 'expired', 'failed'].includes(latest.status)) throw error;
        }
      }
      if (current()) updateUpload(id, { status: 'canceled', cancelRequested: false, control: null, pendingParts: null, creation: null });
    } catch (error) {
      if (current()) updateUpload(id, { status: 'cancel_pending', control: null, creation: running?.creation || task.creation, error: `取消待确认：${error.message}` });
    } finally { if (cancelControls.current.get(id) === controller) cancelControls.current.delete(id); }
  }, [updateUpload]);

  const removeUpload = useCallback(id => {
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task || !['failed', 'canceled'].includes(task.status)) return;
    const runOwner = ownerId.current;
    const remove = () => {
      if (ownerId.current !== runOwner) return;
      releasePendingParts(task.pendingParts);
      const next = uploadsRef.current.filter(item => item.id !== id);
      uploadsRef.current = next;
      setUploads(next);
    };
    if (task.status === 'failed' && (task.session || task.creation)) {
      // Resolve uncertain creation and cancellation before discarding the only local recovery record.
      cancelUpload(id).then(() => {
        if (uploadsRef.current.find(item => item.id === id)?.status === 'canceled') remove();
      });
      return;
    }
    remove();
  }, [cancelUpload]);

  const activeCount = uploads.filter(task => activeStatuses.has(task.status)).length;
  const value = useMemo(() => ({
    uploads,
    activeCount,
    enqueueUploads,
    pauseUpload,
    resumeUpload,
    selectResumeFile,
    cancelUpload,
    removeUpload,
  }), [activeCount, cancelUpload, enqueueUploads, pauseUpload, removeUpload, resumeUpload, selectResumeFile, uploads]);

  return <TransferContext.Provider value={value}>{children}</TransferContext.Provider>;
};

export const useTransfers = () => useContext(TransferContext);
