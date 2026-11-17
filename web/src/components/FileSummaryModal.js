import React, { useEffect, useMemo, useState } from 'react';
import { Modal, Skeleton, Tag } from 'antd';
import { FileTextOutlined, InfoCircleOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import { getFileAI } from '../services/ai';

export const AI_SUMMARY_LIMIT = 240;

export const truncateSummary = value => {
  const text = String(value || '').trim();
  const runes = Array.from(text);
  if (runes.length <= AI_SUMMARY_LIMIT) return text;
  return `${runes.slice(0, AI_SUMMARY_LIMIT - 1).join('')}…`;
};

const SummaryModal = styled(Modal)`
  .ant-modal-content {
    padding: 24px;
    border: 1px solid var(--divider-strong);
    border-radius: var(--radius-panel);
    background: var(--white);
    box-shadow: var(--shadow-panel);
  }

  .ant-modal-header { margin-bottom: 0; padding-bottom: 16px; border-bottom: 1px solid var(--divider); background: transparent; }
  .ant-modal-title { padding-right: 28px; color: var(--ink); font-family: var(--body-font); font-size: 16px; font-weight: 600; line-height: 1.5; }

  @media (max-width: 620px) {
    && { max-width: calc(100% - 32px); }
    .ant-modal-content { padding: 20px; }
  }
`;

const ModalTitle = styled.div`
  display: flex;
  gap: 8px;
  align-items: flex-start;

  .anticon { flex: 0 0 auto; margin-top: 4px; color: var(--text-secondary); }
  span:last-child { min-width: 0; overflow-wrap: anywhere; }
`;

const Content = styled.div`
  display: grid;
  gap: 16px;
  align-content: start;
  max-height: min(65vh, 560px);
  overflow-y: auto;
  padding-top: 20px;
`;

const Unavailable = styled.div`
  display: grid;
  min-height: 112px;
  gap: 12px;
  align-content: center;
  justify-items: center;
  padding: 28px 16px;
  border: 1px solid var(--divider);
  border-radius: var(--radius-control);
  color: var(--ink);
  background: var(--surface-soft);
  text-align: center;

  > .anticon { color: var(--text-secondary); font-size: 24px; }
  p { margin: 0; font-size: 15px; font-weight: 500; line-height: 1.7; }
`;

const Summary = styled.p`
  margin: 0;
  padding: 4px 0 4px 18px;
  border-left: 3px solid var(--lime);
  color: var(--ink);
  font-size: 15px;
  line-height: 1.85;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
`;

const Metadata = styled.div`
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  align-items: center;

  .ant-tag {
    max-width: 100%;
    margin: 0;
    padding: 3px 9px;
    border-color: var(--divider-strong);
    border-radius: var(--radius-control);
    color: var(--text-secondary);
    background: var(--surface-soft);
    font-size: 12px;
    line-height: 18px;
    white-space: normal;
    overflow-wrap: anywhere;
  }

  .overview-label { color: var(--ink); background: var(--mint); }
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
  const indexed = (document?.ai_status || document?.status) === 'indexed';
  const available = Boolean(summary) && indexed;

  return <SummaryModal
    open={open}
    title={<ModalTitle><FileTextOutlined /><span title={file?.name}>{file?.name || '文档摘要'}</span></ModalTitle>}
    width={600}
    centered
    destroyOnClose
    onCancel={onClose}
    footer={null}
  >
    <Content as="article" aria-label="AI 概览" aria-live="polite" aria-busy={loading}>
      {loading && <Skeleton active paragraph={{ rows: 5 }} title={false} />}
      {!loading && (error || !available) && <Unavailable>
        <InfoCircleOutlined aria-hidden="true" />
        <p>{error || unavailableMessage}</p>
      </Unavailable>}
      {!loading && available && <>
        <Summary>{summary}</Summary>
        <Metadata aria-label="概览标签">
          <Tag className="overview-label">AI 概览</Tag>
          {document.language && <Tag>{document.language.toUpperCase()}</Tag>}
          {document.tags?.map(tag => <Tag key={tag}>{tag}</Tag>)}
        </Metadata>
      </>}
    </Content>
  </SummaryModal>;
};

export default FileSummaryModal;
