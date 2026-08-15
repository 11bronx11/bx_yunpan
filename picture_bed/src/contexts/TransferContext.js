import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import { abortUpload, listActiveUploads, uploadFile, waitForUpload } from '../services/drive';
import { useAuth } from './AuthContext';

const TransferContext = createContext(null);

export const MAX_BATCH_FILES = 10;
export const MAX_CONCURRENT_UPLOADS = 4;

const terminalStatuses = new Set(['completed', 'skipped', 'failed', 'canceled']);
const persistedStatuses = new Set(['queued', 'checking', 'uploading', 'pausing', 'paused', 'verifying', 'verification_pending', 'interrupted']);
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
});

const sessionProgress = session => {
  if (session.status === 'verifying') return 90;
  const bytes = (session.confirmed_parts || []).reduce((total, part) => total + part.size_bytes, 0);
  return Math.round(Math.min(bytes / session.size_bytes, 1) * 90);
};

const recoveredUpload = (session, local) => {
  const progress = sessionProgress(session);
  return {
    id: local?.id || `upload-${session.id}`,
    file: null,
    folderId: session.folder_id,
    destination: local?.destination || '目标目录',
    name: session.filename,
    size: session.size_bytes,
    mimeType: local?.mimeType || 'application/octet-stream',
    status: session.status === 'verifying' ? 'verifying' : 'interrupted',
    progress,
    transferred: Math.round(session.size_bytes * progress / 100),
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
  status: 'interrupted',
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
  const ownerId = useRef(null);
  const hydratedOwner = useRef(null);

  useEffect(() => { uploadsRef.current = uploads; }, [uploads]);

  useEffect(() => () => {
    uploadsRef.current.forEach(task => {
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
      uploadsRef.current.forEach(task => {
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
    const local = readStoredUploads(nextOwnerId);
    const localBySession = new Map(local.filter(task => task.session?.id).map(task => [task.session.id, task]));
    listActiveUploads()
      .then(response => {
        if (!mounted || ownerId.current !== nextOwnerId) return;
        const sessions = response.items || [];
        const backendTasks = sessions.map(session => recoveredUpload(session, localBySession.get(session.id)));
        const localOnly = local.filter(task => !task.session?.id).map(localInterruptedUpload);
        hydratedOwner.current = nextOwnerId;
        setUploads([...backendTasks, ...localOnly]);
      })
      .catch(() => {
        if (!mounted || ownerId.current !== nextOwnerId) return;
        hydratedOwner.current = nextOwnerId;
        setUploads(local.map(localInterruptedUpload));
      });
    return () => { mounted = false; };
  }, [ready, user?.id]);

  useEffect(() => {
    if (!ready || !user?.id || hydratedOwner.current !== user.id) return;
    const records = uploads.filter(task => persistedStatuses.has(task.status)).map(storedUpload);
    try {
      if (records.length > 0) localStorage.setItem(storageKey(user.id), JSON.stringify(records));
      else localStorage.removeItem(storageKey(user.id));
    } catch {
      // Uploads still work in-memory when browser storage is unavailable.
    }
  }, [ready, uploads, user?.id]);

  useEffect(() => {
    const hasBrowserUpload = uploads.some(task => task.file && ['queued', 'checking', 'uploading', 'pausing'].includes(task.status));
    if (!hasBrowserUpload) return undefined;
    const warnBeforeUnload = event => {
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', warnBeforeUnload);
    return () => window.removeEventListener('beforeunload', warnBeforeUnload);
  }, [uploads]);

  const updateUpload = useCallback((id, change) => {
    setUploads(current => current.map(task => {
      if (task.id !== id) return task;
      return { ...task, ...(typeof change === 'function' ? change(task) : change) };
    }));
  }, []);

  const runUpload = useCallback(async id => {
    if (activeUploads.current.has(id)) return;
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task || !['queued', 'verifying'].includes(task.status)) return;
    if (task.status === 'queued' && !task.file) {
      updateUpload(id, { status: 'interrupted', error: null });
      return;
    }

    activeUploads.current.add(id);
    const abortController = new AbortController();
    const control = {
      paused: false,
      canceled: false,
      signal: abortController.signal,
      abortController,
      idempotencyKey: task.idempotencyKey,
      isPaused: () => control.paused,
      isCanceled: () => control.canceled,
      onSession: session => updateUpload(id, current => ({
        session,
        status: session.status === 'verifying' ? 'verifying' : (current.status === 'checking' ? 'uploading' : current.status),
      })),
    };
    const startedAt = Date.now();
    let lastBytes = Math.round(task.size * (task.progress || 0) / 100);
    let lastTime = startedAt;
    const onProgress = progress => {
      const now = Date.now();
      const transferred = Math.round(task.size * progress / 100);
      const elapsed = Math.max(now - lastTime, 1) / 1000;
      const speed = Math.max(0, Math.round((transferred - lastBytes) / elapsed));
      lastBytes = transferred;
      lastTime = now;
      updateUpload(id, { progress, transferred, speed });
    };

    updateUpload(id, {
      status: task.status === 'verifying' ? 'verifying' : 'checking',
      control,
      error: null,
      startedAt,
      speed: 0,
    });
    try {
      const result = task.status === 'verifying'
        ? await waitForUpload(task.session, onProgress, control)
        : await uploadFile(task.file, task.folderId, onProgress, control, task.session);

      if (control.canceled) return;
      if (result.paused) {
        updateUpload(id, { status: 'paused', session: result.session, control: null, speed: 0 });
        return;
      }
      updateUpload(id, {
        status: 'completed',
        progress: 100,
        transferred: task.size,
        speed: 0,
        control: null,
        session: result.session || task.session,
        instant: Boolean(result.instant),
        completedAt: Date.now(),
      });
    } catch (error) {
      if (control.canceled) return;
      if (error.code === 'upload.file_exists') {
        updateUpload(id, { status: 'skipped', progress: 100, transferred: 0, speed: 0, control: null, error: error.message });
      } else if (error.code === 'client.file_mismatch') {
        updateUpload(id, { status: 'interrupted', file: null, speed: 0, control: null, error: error.message });
      } else if (error.code === 'client.verify_timeout') {
        updateUpload(id, { status: 'verification_pending', speed: 0, control: null, error: error.message });
      } else {
        const serverTerminal = ['failed', 'aborted', 'expired'].includes(error.uploadStatus);
        updateUpload(id, {
          status: error.name === 'AbortError' ? 'canceled' : 'failed',
          error: error.name === 'AbortError' ? null : (error.message || '上传失败，请重试'),
          control: null,
          speed: 0,
          ...(serverTerminal ? {
            session: null,
            idempotencyKey: `upload-${randomId()}`,
            progress: 0,
            transferred: 0,
          } : {}),
        });
      }
    } finally {
      activeUploads.current.delete(id);
      setUploads(current => [...current]);
    }
  }, [updateUpload]);

  useEffect(() => {
    const available = MAX_CONCURRENT_UPLOADS - activeUploads.current.size;
    if (available <= 0) return;
    uploads.filter(task => ['queued', 'verifying'].includes(task.status) && !activeUploads.current.has(task.id))
      .slice(0, available)
      .forEach(task => runUpload(task.id));
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
    if (task.status === 'uploading' && task.control) {
      task.control.paused = true;
      updateUpload(id, { status: 'pausing', speed: 0 });
    }
  }, [updateUpload]);

  const resumeUpload = useCallback(id => {
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task || !['paused', 'failed', 'verification_pending'].includes(task.status)) return;
    const status = task.session?.status === 'verifying' ? 'verifying' : 'queued';
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

  const cancelUpload = useCallback(id => {
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task || terminalStatuses.has(task.status)) return;
    if (task.control) {
      task.control.canceled = true;
      task.control.abortController.abort();
    }
    updateUpload(id, { status: 'canceled', control: null, speed: 0, error: null });
    if (task.session?.id && task.session.status !== 'verifying') {
      abortUpload(task.session.id).catch(error => {
        updateUpload(id, { status: 'failed', error: `取消上传失败：${error.message}` });
      });
    }
  }, [updateUpload]);

  const removeUpload = useCallback(id => {
    const task = uploadsRef.current.find(item => item.id === id);
    if (!task || !['failed', 'canceled'].includes(task.status)) return;
    const remove = () => setUploads(current => current.filter(item => item.id !== id));
    if (task.status === 'failed' && task.session?.id && task.session.status !== 'verifying') {
      abortUpload(task.session.id).then(remove).catch(error => updateUpload(id, { error: `移除失败：${error.message}` }));
      return;
    }
    remove();
  }, [updateUpload]);

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
