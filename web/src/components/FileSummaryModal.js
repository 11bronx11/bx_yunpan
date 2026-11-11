import React, { useEffect, useMemo, useState } from 'react';
import { Button, Modal, Skeleton, Tag } from 'antd';
import { FileTextOutlined, InfoCircleOutlined } from '@ant-design/icons';
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

const SummaryModal = styled(Modal)`
  .ant-modal-content {
    padding: 24px;
    border: 1px solid var(--divider-strong);
    border-radius: var(--radius-panel);
    background: var(--white);
    box-shadow: var(--shadow-panel);
  }

  .ant-modal-header { margin-bottom: 0; padding-bottom: 16px; border-bottom: 1px solid var(--divider); background: transparent; }
  .ant-modal-title { padding-right: 28px; color: var(--ink); font-family: var(--body-font); font-size: 16px; font-weight: 600; }
  .ant-modal-footer { margin-top: 20px; }
  .ant-modal-footer .ant-btn {
    min-width: 80px;
    min-height: 40px;
    padding-inline: 20px;
    border-color: var(--control-border);
    border-radius: var(--radius-pill);
    color: var(--ink);
    background: var(--white);
    box-shadow: none;
  }
  .ant-modal-footer .ant-btn:hover { border-color: var(--ink); color: var(--ink); background: var(--surface-hover); }

  @media (max-width: 620px) {
    && { max-width: calc(100% - 32px); }
    .ant-modal-content { padding: 20px; }
    .ant-modal-footer .ant-btn { min-height: 44px; }
  }
`;

const ModalTitle = styled.div`
  display: flex;
  gap: 8px;
  align-items: center;

  .anticon { flex: 0 0 auto; color: var(--text-secondary); }
  span:last-child { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
`;

const Content = styled.div`
  display: grid;
  gap: 18px;
  align-content: start;
  padding-top: 20px;
`;

const Unavailable = styled.div`
  display: grid;
  min-height: 160px;
  gap: 14px;
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

  return <SummaryModal
    open={open}
    title={<ModalTitle><FileTextOutlined /><span title={file?.name}>{file?.name || '文档摘要'}</span></ModalTitle>}
    width={560}
    centered
    destroyOnClose
    onCancel={onClose}
    footer={<Button onClick={onClose}>关闭</Button>}
  >
    <Content aria-live="polite" aria-busy={loading}>
      {loading && <Skeleton active paragraph={{ rows: 5 }} title={false} />}
      {!loading && (error || !available) && <Unavailable>
        <InfoCircleOutlined aria-hidden="true" />
        <p>{unavailableMessage}</p>
      </Unavailable>}
      {!loading && available && <>
        <Summary>{summary}</Summary>
        <Metadata>
          <StatusTag tone="mint">AI 摘要</StatusTag>
          {document.language && <Tag>{document.language.toUpperCase()}</Tag>}
          {document.tags?.map(tag => <Tag key={tag}>{tag}</Tag>)}
        </Metadata>
      </>}
    </Content>
  </SummaryModal>;
};

export default FileSummaryModal;
