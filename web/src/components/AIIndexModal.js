import React, { useCallback, useEffect, useRef, useState } from 'react';
import { App, Alert, Button, Modal, Progress, Space } from 'antd';
import { ReloadOutlined, SyncOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import { getAIJob, getFileAI, reprocessFileAI } from '../services/ai';
import { createPollDelay } from '../services/polling';
import { StatusTag } from './design/ProductUI';

const Detail = styled.div`
  display: grid;
  gap: 18px;
  padding-top: 4px;
`;

const StatusRow = styled.div`
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  align-items: center;
  justify-content: space-between;
  min-height: 40px;
  padding-bottom: 14px;
  border-bottom: 1px solid var(--divider);
`;

const Metadata = styled.dl`
  display: grid;
  grid-template-columns: 96px minmax(0, 1fr);
  gap: 10px 16px;
  margin: 0;

  dt { color: var(--muted); font-size: 13px; }
  dd { min-width: 0; margin: 0; overflow-wrap: anywhere; font-size: 13px; }

  @media (max-width: 520px) {
    grid-template-columns: 1fr;
    gap: 4px;
    dd { margin-bottom: 8px; }
  }
`;

const Summary = styled.p`
  margin: 0;
  color: var(--ink);
  line-height: 1.7;
  white-space: pre-wrap;
`;

const statusMeta = {
  missing: { label: '尚未建立', tone: undefined },
  pending: { label: '等待构建', tone: 'blue' },
  processing: { label: '正在构建', tone: 'blue' },
  indexed: { label: '已完成', tone: 'mint' },
  failed: { label: '构建失败', tone: 'coral' },
  unsupported: { label: '构建失败', tone: 'coral' },
  canceled: { label: '已取消', tone: undefined },
  unconfirmed: { label: '结果待确认', tone: undefined },
};

const isActiveTask = task => task && (task.status === 'pending' || task.status === 'running');
const isFailedIndex = status => status === 'failed' || status === 'unsupported';
const buildMessageKey = 'index-build';

const AIIndexModal = ({ file, open, onClose, onStatusChange }) => {
  const { message } = App.useApp();
  const [document, setDocument] = useState(null);
  const [missing, setMissing] = useState(false);
  const [loading, setLoading] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [task, setTask] = useState(null);
  const [loadError, setLoadError] = useState('');
  const view = useRef(null);
  const submissionPending = useRef(false);
  const activeTaskID = isActiveTask(task) ? task.id : null;

  const refreshDocument = useCallback(async (signal, quiet = false) => {
    if (!file?.id) return null;
    if (!quiet) setLoading(true);
    setLoadError('');
    try {
      const value = await getFileAI(file.id, { signal });
      if (signal?.aborted) return null;
      setDocument(value);
      setMissing(false);
      setTask(current => current?.status === 'succeeded' ? { ...current, resultStatus: value.status } : current);
      onStatusChange?.(file.id, value.status);
      return value;
    } catch (error) {
      if (error.name === 'AbortError' || signal?.aborted) return null;
      if (error.status === 404) {
        setDocument(null);
        setMissing(true);
        onStatusChange?.(file.id, null);
        return null;
      }
      setLoadError('索引状态加载失败，请稍后重试。');
      return null;
    } finally {
      if (!quiet && !signal?.aborted) setLoading(false);
    }
  }, [file?.id, onStatusChange]);

  useEffect(() => {
    if (!open || !file?.id) return undefined;
    const controller = new AbortController();
    view.current = controller;
    submissionPending.current = false;
    setSubmitting(false);
    setTask(null);
    setDocument(null);
    setMissing(false);
    refreshDocument(controller.signal);
    return () => {
      controller.abort();
      view.current = null;
      message.destroy(buildMessageKey);
    };
  }, [open, file?.id, message, refreshDocument]);

  useEffect(() => {
    if (!open || !activeTaskID) return undefined;
    let stopped = false;
    let timer;
    const controller = new AbortController();
    const nextDelay = createPollDelay();
    const poll = async () => {
      try {
        const current = await getAIJob(activeTaskID, { signal: controller.signal });
        if (stopped) return;
        setLoadError('');
        if (current.status === 'succeeded' || current.status === 'failed' || current.status === 'canceled') {
          const result = await refreshDocument(controller.signal, true);
          if (stopped) return;
          setTask({ ...current, resultStatus: result?.status });
          if (current.status === 'failed' || (current.status === 'succeeded' && isFailedIndex(result?.status))) {
            message.open({ key: buildMessageKey, type: 'error', content: '构建失败' });
          } else if (current.status === 'canceled') {
            message.open({ key: buildMessageKey, type: 'info', content: '构建已取消' });
          } else if (result?.status === 'indexed') {
            message.open({ key: buildMessageKey, type: 'success', content: '索引构建完成' });
          } else {
            message.open({ key: buildMessageKey, type: 'info', content: '暂未确认构建结果，请刷新状态。' });
          }
          return;
        }
        setTask(current);
        timer = window.setTimeout(poll, nextDelay(`${current.status}:${current.progress}:${current.attempt}`, current.poll_after_ms));
      } catch (error) {
        if (stopped || error.name === 'AbortError') return;
        if (!error.status || error.status >= 500 || error.status === 429) {
          setLoadError('索引状态获取失败，正在重试。');
          timer = window.setTimeout(poll, 5000);
        } else {
          setTask(null);
          setLoadError('索引状态获取失败，请刷新状态。');
        }
      }
    };
    timer = window.setTimeout(poll, 600);
    return () => {
      stopped = true;
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [activeTaskID, message, open, refreshDocument]);

  const startReprocess = async () => {
    const controller = view.current;
    if (!file?.id || !controller || controller.signal.aborted || submissionPending.current || isActiveTask(task)) return;
    submissionPending.current = true;
    setSubmitting(true);
    setLoadError('');
    try {
      const value = await reprocessFileAI(file.id);
      if (controller.signal.aborted) return;
      setTask(value);
      message.open({ key: buildMessageKey, type: 'success', content: '索引构建请求已提交' });
    } catch (error) {
      if (!controller.signal.aborted) message.open({ key: buildMessageKey, type: 'error', content: '构建失败' });
    } finally {
      if (!controller.signal.aborted) {
        submissionPending.current = false;
        setSubmitting(false);
      }
    }
  };

  // A completed request is only a successful build when the index is confirmed.
  // In particular, older workers can report succeeded with an unsupported index.
  let status = document?.status || (missing ? 'missing' : 'pending');
  if (isActiveTask(task)) status = task.status === 'pending' ? 'pending' : 'processing';
  else if (task?.status === 'failed' || task?.status === 'canceled') status = task.status;
  else if (task?.status === 'succeeded') {
    status = task.resultStatus === 'indexed' ? 'indexed' : isFailedIndex(task.resultStatus) ? 'failed' : 'unconfirmed';
  }
  const meta = statusMeta[status] || statusMeta.pending;
  const active = submitting || isActiveTask(task) || status === 'processing' || status === 'pending';
  const failed = isFailedIndex(status);
  const progressStatus = failed ? 'exception' : status === 'indexed' ? 'success' : isActiveTask(task) ? 'active' : 'normal';

  return (
    <Modal
      open={open}
      title={`AI 索引${file?.name ? ` · ${file.name}` : ''}`}
      width={620}
      destroyOnClose
      onCancel={onClose}
      footer={[
        <Button key="close" onClick={onClose}>关闭</Button>,
        <Button key="refresh" aria-label="刷新 AI 索引状态" icon={<ReloadOutlined />} disabled={loading || submitting || isActiveTask(task)} onClick={() => refreshDocument(view.current?.signal)}>刷新状态</Button>,
        <Button key="reprocess" aria-label={missing ? '构建索引' : '重新构建索引'} type="primary" icon={<SyncOutlined />} loading={submitting || isActiveTask(task)} disabled={loading || active} onClick={startReprocess}>
          {missing ? '构建索引' : '重新构建索引'}
        </Button>,
      ]}
    >
      <Detail aria-live="polite" aria-busy={loading || isActiveTask(task)}>
        <StatusRow>
          <Space><span>当前状态</span><StatusTag tone={meta.tone}>{loading ? '正在加载' : meta.label}</StatusTag></Space>
        </StatusRow>
        {loadError && <Alert type="error" showIcon message={loadError} />}
        {failed && <Alert type="error" showIcon message="构建失败" />}
        {task && <Progress percent={task.progress || 0} status={progressStatus} />}
        {document && <Metadata>
          <dt>模型</dt><dd>{document.model_version || '-'}</dd>
          <dt>索引管线</dt><dd>{document.pipeline_version || '-'}</dd>
          <dt>语言</dt><dd>{document.language || '-'}</dd>
          <dt>标签</dt><dd>{document.tags?.length ? document.tags.join(' · ') : '-'}</dd>
        </Metadata>}
        {document?.summary && <Summary>{document.summary}</Summary>}
      </Detail>
    </Modal>
  );
};

export default AIIndexModal;
