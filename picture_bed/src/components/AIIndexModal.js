import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { App, Alert, Button, Modal, Progress, Space } from 'antd';
import { ReloadOutlined, SyncOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import { getAIJob, getFileAI, reprocessFileAI } from '../services/ai';
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
  pending: { label: '等待处理', tone: 'blue' },
  processing: { label: '正在处理', tone: 'blue' },
  indexed: { label: '已完成', tone: 'mint' },
  failed: { label: '构建失败', tone: 'coral' },
  unsupported: { label: '不支持', tone: 'coral' },
};

const errorMessages = {
  'ai.object_too_large': '文件超过 AI 处理大小上限，请调整 AI_MAX_OBJECT_MIB 后重建。',
  'ai.unsupported_type': '当前 Provider 不支持该文件类型，或图片超过 10 MiB。',
  'ai.invalid_content': '文件内容无法解析，请确认文件没有损坏。',
  'ai.empty_content': '文件中没有提取到可索引内容。',
  'ai.storage_read_failed': '读取对象存储失败，请稍后重试。',
  'ai.extraction_failed': '文本或图片内容提取失败，请稍后重试。',
  'ai.embedding_failed': '向量模型调用失败，请检查模型配置后重试。',
  'ai.enrichment_failed': '摘要模型调用失败，请检查模型配置后重试。',
  'ai.persistence_failed': '索引写入失败，请稍后重试。',
  'ai.reprocess_failed': '索引重建失败，请检查 Worker 日志后重试。',
};

const isActiveTask = task => task && (task.status === 'pending' || task.status === 'running');

const AIIndexModal = ({ file, open, onClose, onStatusChange }) => {
  const { message } = App.useApp();
  const [document, setDocument] = useState(null);
  const [missing, setMissing] = useState(false);
  const [loading, setLoading] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [task, setTask] = useState(null);
  const [loadError, setLoadError] = useState('');
  const activeTaskID = isActiveTask(task) ? task.id : null;

  const refreshDocument = useCallback(async (signal, quiet = false) => {
    if (!file?.id) return null;
    if (!quiet) setLoading(true);
    setLoadError('');
    try {
      const value = await getFileAI(file.id, { signal });
      setDocument(value);
      setMissing(false);
      onStatusChange?.(file.id, value.status);
      return value;
    } catch (error) {
      if (error.name === 'AbortError') return null;
      if (error.status === 404) {
        setDocument(null);
        setMissing(true);
        onStatusChange?.(file.id, null);
        return null;
      }
      setLoadError(error.message || 'AI 索引状态加载失败');
      return null;
    } finally {
      if (!quiet) setLoading(false);
    }
  }, [file?.id, onStatusChange]);

  useEffect(() => {
    if (!open || !file?.id) return undefined;
    const controller = new AbortController();
    setTask(null);
    setDocument(null);
    setMissing(false);
    refreshDocument(controller.signal);
    return () => controller.abort();
  }, [open, file?.id, refreshDocument]);

  useEffect(() => {
    if (!open || !activeTaskID) return undefined;
    let stopped = false;
    let timer;
    const poll = async () => {
      try {
        const current = await getAIJob(activeTaskID);
        if (stopped) return;
        if (current.status === 'succeeded' || current.status === 'failed') {
          const result = await refreshDocument(undefined, true);
          if (stopped) return;
          setTask(current);
          if (current.status === 'failed') {
            message.error(current.error_message || 'AI 索引重建失败');
          } else if (result?.status === 'indexed') {
            message.success('AI 索引已重建');
          } else {
            message.warning('处理已完成，但该文件仍无法建立 AI 索引');
          }
          return;
        }
        setTask(current);
        timer = window.setTimeout(poll, 1200);
      } catch (error) {
        if (!stopped) message.error(error.message || 'AI 任务状态获取失败');
      }
    };
    timer = window.setTimeout(poll, 600);
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, [activeTaskID, message, open, refreshDocument]);

  const startReprocess = async () => {
    if (!file?.id) return;
    setSubmitting(true);
    try {
      const value = await reprocessFileAI(file.id);
      setTask(value);
      message.success('已加入 AI 索引队列');
    } catch (error) {
      message.error(error.message || 'AI 索引任务创建失败');
    } finally {
      setSubmitting(false);
    }
  };

  const status = document?.status || (missing ? 'missing' : 'pending');
  const meta = statusMeta[status] || statusMeta.pending;
  const active = isActiveTask(task) || status === 'processing' || status === 'pending';
  const feedback = useMemo(() => {
    const code = task?.error_code || document?.error_code;
    return code ? errorMessages[code] || task?.error_message || `处理失败：${code}` : '';
  }, [document?.error_code, task?.error_code, task?.error_message]);

  return (
    <Modal
      open={open}
      title={`AI 索引${file?.name ? ` · ${file.name}` : ''}`}
      width={620}
      destroyOnClose
      onCancel={onClose}
      footer={[
        <Button key="close" onClick={onClose}>关闭</Button>,
        <Button key="refresh" aria-label="刷新 AI 索引状态" icon={<ReloadOutlined />} disabled={loading || isActiveTask(task)} onClick={() => refreshDocument()}>刷新状态</Button>,
        <Button key="reprocess" aria-label={missing ? '构建 AI 索引' : '重新构建索引'} type="primary" icon={<SyncOutlined />} loading={submitting || isActiveTask(task)} disabled={loading || active} onClick={startReprocess}>
          {missing ? '构建 AI 索引' : '重新构建索引'}
        </Button>,
      ]}
    >
      <Detail aria-live="polite" aria-busy={loading || isActiveTask(task)}>
        <StatusRow>
          <Space><span>当前状态</span><StatusTag tone={meta.tone}>{loading ? '正在加载' : meta.label}</StatusTag></Space>
          {task && <span style={{ color: 'var(--muted)', fontSize: 12 }}>第 {task.attempt || 0} 次处理</span>}
        </StatusRow>
        {loadError && <Alert type="error" showIcon message={loadError} />}
        {feedback && <Alert type={status === 'unsupported' ? 'warning' : 'error'} showIcon message={feedback} />}
        {task && <Progress percent={task.progress || 0} status={task.status === 'failed' ? 'exception' : task.status === 'succeeded' ? 'success' : 'active'} />}
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
