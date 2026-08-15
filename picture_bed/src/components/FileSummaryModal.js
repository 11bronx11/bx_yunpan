import React, { useEffect, useMemo, useState } from 'react';
import { Alert, Button, Modal, Skeleton, Space, Tag } from 'antd';
import { FileTextOutlined, RobotOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import { getFileAI } from '../services/ai';
import { StatusTag } from './design/ProductUI';

export const AI_SUMMARY_LIMIT = 240;

export const truncateSummary = value => {
  const text = String(value || '').trim();
  const runes = Array.from(text);
  if (runes.length <= AI_SUMMARY_LIMIT) return text;
  return `${runes.slice(0, AI_SUMMARY_LIMIT - 1).join('')}…`;
};

const Content = styled.div`
  display: grid;
  min-height: 210px;
  gap: 18px;
  align-content: start;
  padding-top: 4px;
`;

const Summary = styled.p`
  margin: 0;
  padding: 4px 0 4px 18px;
  border-left: 3px solid var(--lime);
  color: var(--ink);
  font-size: 15px;
  line-height: 1.85;
  white-space: pre-wrap;
`;

const Metadata = styled.div`
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
`;

const statusMeta = {
  pending: { label: '等待处理', tone: 'blue', message: '文档已进入 AI 索引队列。' },
  processing: { label: '处理中', tone: 'blue', message: 'AI 摘要正在生成，请稍后重试。' },
  failed: { label: '构建失败', tone: 'coral', message: 'AI 摘要生成失败，请重建索引。' },
  unsupported: { label: '未索引', tone: 'coral', message: '该文档暂时没有可用的 AI 摘要。' },
};

const FileSummaryModal = ({ file, open, onClose, onManageIndex }) => {
  const [document, setDocument] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    if (!open || !file?.id) return undefined;
    const controller = new AbortController();
    let active = true;
    setDocument(null);
    setError('');
    setLoading(true);
    getFileAI(file.id, { signal: controller.signal })
      .then(value => { if (active) setDocument(value); })
      .catch(value => {
        if (!active || value.name === 'AbortError') return;
        if (value.status === 404) setError('该文档还没有建立 AI 摘要。');
        else setError(value.message || 'AI 摘要加载失败，请稍后重试。');
      })
      .finally(() => { if (active) setLoading(false); });
    return () => {
      active = false;
      controller.abort();
    };
  }, [file?.id, open]);

  const summary = useMemo(() => truncateSummary(document?.summary), [document?.summary]);
  const status = statusMeta[document?.status];
  const manageAction = onManageIndex ? <Button aria-label="管理索引" icon={<RobotOutlined />} onClick={() => onManageIndex(file)}>管理索引</Button> : null;

  return <Modal
    open={open}
    title={<Space><FileTextOutlined /><span>{file?.name || '文档摘要'}</span></Space>}
    width={640}
    centered
    destroyOnClose
    onCancel={onClose}
    footer={<Button onClick={onClose}>关闭</Button>}
  >
    <Content aria-live="polite" aria-busy={loading}>
      {loading && <Skeleton active paragraph={{ rows: 5 }} title={false} />}
      {!loading && error && <Alert type="info" showIcon message={error} action={manageAction} />}
      {!loading && document && status && <Metadata><StatusTag tone={status.tone}>{status.label}</StatusTag></Metadata>}
      {!loading && document && document.status !== 'indexed' && <Alert type={document.status === 'failed' ? 'error' : 'info'} showIcon message={status?.message || 'AI 摘要暂不可用。'} action={manageAction} />}
      {!loading && document?.status === 'indexed' && !summary && <Alert type="info" showIcon message="该文档没有生成可用摘要。" action={manageAction} />}
      {!loading && document?.status === 'indexed' && summary && <>
        <Summary>{summary}</Summary>
        <Metadata>
          <StatusTag tone="mint">AI 摘要</StatusTag>
          {document.language && <Tag>{document.language.toUpperCase()}</Tag>}
          {document.tags?.map(tag => <Tag key={tag}>{tag}</Tag>)}
        </Metadata>
      </>}
    </Content>
  </Modal>;
};

export default FileSummaryModal;
