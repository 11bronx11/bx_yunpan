import React, { useEffect, useState } from 'react';
import { Alert, Button } from 'antd';
import { useLocation, useNavigate } from 'react-router-dom';
import Input from '../components/IMEInput';
import { FolderOpenOutlined, InfoCircleOutlined, SendOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import AskFilePicker from '../components/AskFilePicker';
import { useAsk } from '../contexts/AskContext';
import { GraphicEmpty, PageHeader, ProductPage } from '../components/design/ProductUI';

const Workspace = styled.div`width: 100%; margin: 0;`;
const Composer = styled.section`
  display: grid;
  gap: 10px;

  .ant-input { min-height: 104px; padding: 14px 16px; resize: vertical; line-height: 1.7; }
`;
const ComposerActions = styled.div`display: flex; flex-wrap: wrap; justify-content: flex-end; align-items: center; gap: 8px;`;
const ScopeNotice = styled(Alert)`
  justify-self: start;
  width: fit-content;
  max-width: 100%;
  align-items: flex-start;
  padding: 6px 14px;
  border: 1px solid #ead8ad;
  border-radius: 18px;
  color: #805b16;
  background: #fff9eb;
  text-align: left;
  overflow-wrap: anywhere;

  .ant-alert-icon { margin-top: 4px; margin-inline-end: 8px; color: #a87c22; font-size: 14px; }
  .ant-alert-content { min-width: 0; }
  .ant-alert-message { color: inherit; font-size: 13px; font-weight: 500; line-height: 22px; }
`;
const SelectionCount = styled.span`
  margin-right: 4px;
  color: var(--text-secondary);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;

  strong { color: var(--ink); font-weight: 600; }
`;
const ResultSection = styled.section`margin-top: 24px;`;
const ResultTitle = styled.h2`
  margin: 0 0 14px;
  font-size: 18px;
  font-weight: 600;
  line-height: 1.4;
`;
const Answer = styled.article`
  padding: 4px 0 4px 18px;
  border-left: 3px solid var(--blue);
  white-space: pre-wrap;
  font-size: 15px;
  line-height: 1.8;
  overflow-wrap: anywhere;
`;

const AIAsk = () => {
  const { question, setQuestion, answer, asking, selected, scopeError, askError,
    preparing, applySelection, prepareFile, runAsk, markViewed } = useAsk();
  const { search } = useLocation();
  const navigate = useNavigate();
  const [pickerOpen, setPickerOpen] = useState(false);
  useEffect(() => {
    const params = new URLSearchParams(search);
    if (params.get('file')) {
      prepareFile(params.get('file'), params.get('name'));
      // Consume the file shortcut so reloading starts with an empty workspace.
      params.delete('file');
      params.delete('name');
      navigate({ pathname: '/ask', search: params.toString() }, { replace: true });
    }
  }, [navigate, prepareFile, search]);
  useEffect(() => { markViewed(); }, [answer, askError, markViewed, scopeError]);

  return <ProductPage accent="var(--mint)">
    <Workspace>
      <PageHeader title="问答" />
      <Composer className="page-reveal">
        {preparing && <Alert type="info" message="正在准备指定文件的问答文本…" />}
        {scopeError && <ScopeNotice type="warning" showIcon icon={<InfoCircleOutlined />} message={scopeError} />}

        <Input.TextArea
          aria-label="向文件提问"
          value={question}
          onChange={event => setQuestion(event.target.value)}
          onPressEnter={event => {
            if (event.shiftKey) return;
            event.preventDefault();
            runAsk();
          }}
          placeholder="例如：这批资料里有哪些待跟进事项？"
        />
        <ComposerActions>
          <SelectionCount aria-label="当前选择文件数量" role="status" aria-live="polite" aria-atomic="true">已选 <strong>{selected.length}</strong> / 50</SelectionCount>
          <Button aria-label="选择文件" type="primary" icon={<FolderOpenOutlined />} onClick={() => setPickerOpen(true)}>选择文件</Button>
          <Button aria-label="提问" type="primary" icon={<SendOutlined />} loading={asking} disabled={!question.trim() || preparing || Boolean(scopeError)} onClick={runAsk}>提问</Button>
        </ComposerActions>
      </Composer>
      <ResultSection>
        <ResultTitle>回答</ResultTitle>
        {askError && <Alert type="error" message={askError} />}
        {answer?.retrieval?.degraded && <Alert type="info" message="本次使用可用的文本检索生成回答" />}
        {answer?.coverage?.missing_evidence_file_ids?.length > 0 && <Alert type="warning" message="部分所选文件没有找到相关证据，回答未覆盖这些文件。" />}

        {answer
          ? <Answer className="page-reveal">{answer.answer}</Answer>
          : <GraphicEmpty title={asking ? '正在整理答案' : '等待你的问题'} />}
      </ResultSection>
      <AskFilePicker open={pickerOpen} selected={selected} onCancel={() => setPickerOpen(false)} onApply={files => { applySelection(files); setPickerOpen(false); }} />
    </Workspace>
  </ProductPage>;
};

export default AIAsk;
