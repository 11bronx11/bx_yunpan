import React, { useEffect, useMemo, useState } from 'react';
import { Alert, Button, Modal, Skeleton, Space, Tag } from 'antd';
import { FileTextOutlined } from '@ant-design/icons';
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

const unavailableMessage = '暂时无法查看AI摘要';

const FileSummaryModal = ({ file, open, onClose }) => {
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
        setError(unavailableMessage);
      })
      .finally(() => { if (active) setLoading(false); });
    return () => {
      active = false;
      controller.abort();
    };
  }, [file?.id, open]);

  const summary = useMemo(() => truncateSummary(document?.summary), [document?.summary]);
  const available = document?.status === 'indexed' && Boolean(summary);

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
      {!loading && (error || !available) && <Alert type="warning" showIcon message={unavailableMessage} />}
      {!loading && available && <>
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
