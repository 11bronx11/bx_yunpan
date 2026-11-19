import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import { askFiles, getFileAI } from '../services/ai';

const AskContext = createContext(null);

// Mounted above the page routes, and keyed by the authenticated user in App.
export const AskProvider = ({ children }) => {
  const [question, setQuestion] = useState('');
  const [answer, setAnswer] = useState(null);
  const [asking, setAsking] = useState(false);
  const [selected, setSelected] = useState([]);
  const [invalidFiles, setInvalidFiles] = useState([]);
  const [scopeError, setScopeError] = useState('');
  const [askError, setAskError] = useState('');
  const [preparing, setPreparing] = useState(false);
  const [unread, setUnread] = useState(false);
  const request = useRef(null);
  const preparation = useRef(null);
  const selectionRevision = useRef(0);

  const stopPreparation = useCallback(() => {
    preparation.current?.controller.abort();
    clearTimeout(preparation.current?.timer);
  }, []);

  const reset = useCallback(() => {
    request.current?.abort();
    request.current = null;
    stopPreparation();
    preparation.current = null;
    selectionRevision.current++;
    setQuestion('');
    setAnswer(null);
    setAsking(false);
    setSelected([]);
    setInvalidFiles([]);
    setScopeError('');
    setAskError('');
    setPreparing(false);
    setUnread(false);
  }, [stopPreparation]);

  useEffect(() => {
    // Real page navigation/reload clears the session; SPA route changes do not.
    window.addEventListener('pagehide', reset);
    return () => {
      window.removeEventListener('pagehide', reset);
      request.current?.abort();
      stopPreparation();
    };
  }, [reset, stopPreparation]);

  const applySelection = useCallback(files => {
    if (preparation.current) preparation.current.status = 'chosen';
    stopPreparation();
    selectionRevision.current++;
    setSelected(files);
    setPreparing(false);
    setScopeError('');
    setInvalidFiles([]);
  }, [stopPreparation]);

  const prepareFile = useCallback((fileId, name) => {
    // Returning to the same file link must not overwrite a later manual choice.
    const previous = preparation.current;
    if (previous?.fileId === fileId && (previous.status === 'ready' || previous.status === 'chosen'
      || (previous.status === 'preparing' && !previous.controller.signal.aborted))) return;
    stopPreparation();
    const pending = { fileId, controller: new AbortController(), attempts: 0, status: 'preparing' };
    preparation.current = pending;
    setPreparing(true);
    const fail = message => {
      pending.status = 'failed';
      setPreparing(false);
      setScopeError(message);
    };
    const check = async () => {
      try {
        const info = await getFileAI(fileId, { signal: pending.controller.signal });
        if (pending.controller.signal.aborted) return;
        if (info.capabilities?.askable) {
          pending.status = 'ready';
          selectionRevision.current++;
          setSelected([{ id: fileId, name: name || '指定文件' }]);
          setPreparing(false);
          setScopeError('');
          setInvalidFiles([]);
        } else if (++pending.attempts < 60 && info.capabilities?.text?.latest_build?.state !== 'failed') {
          pending.timer = setTimeout(check, 2000);
        } else fail('这个文件尚未准备好问答，请在文件菜单中重建后重试。');
      } catch (error) {
        if (!pending.controller.signal.aborted) fail(error.message || '无法读取指定文件');
      }
    };
    check();
  }, [stopPreparation]);

  const runAsk = useCallback(async () => {
    const value = question.trim();
    // The ref also guards a second submit before React renders the loading state.
    if (!value || request.current || preparing || scopeError) return;
    const controller = new AbortController();
    request.current = controller;
    const revision = selectionRevision.current;
    const scope = selected.length ? { mode: 'selected', file_ids: selected.map(file => file.id) } : { mode: 'all' };
    setAsking(true);
    setQuestion('');
    setAnswer(null);
    setAskError('');
    setUnread(false);
    try {
      const result = await askFiles(value, { scope, signal: controller.signal });
      if (controller.signal.aborted) return;
      setAnswer(result);
      setUnread(true);
    } catch (error) {
      if (controller.signal.aborted) return;
      if (error.code === 'ai.scope_invalid' || error.code === 'ai.scope_changed') {
        setQuestion(current => current || value);
        if (selectionRevision.current === revision) {
          setScopeError(error.message || '选择已失效，请更新文件范围');
          setInvalidFiles(error.invalidFiles || []);
        } else setAskError(error.message || '本次问答的文件范围已变化，请重新提问');
      } else setAskError(error.message || '问答失败，请重试');
      setUnread(true);
    } finally {
      if (request.current === controller) {
        request.current = null;
        if (!controller.signal.aborted) setAsking(false);
      }
    }
  }, [preparing, question, scopeError, selected]);

  const markViewed = useCallback(() => setUnread(false), []);
  const value = useMemo(() => ({
    question, setQuestion, answer, asking, selected, invalidFiles, scopeError, askError,
    preparing, unread, applySelection, prepareFile, runAsk, markViewed, reset,
  }), [answer, applySelection, askError, asking, invalidFiles, markViewed, prepareFile, preparing, question, reset, runAsk, scopeError, selected, unread]);

  return <AskContext.Provider value={value}>{children}</AskContext.Provider>;
};

export const useAsk = () => useContext(AskContext);
