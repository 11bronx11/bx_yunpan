import React, { useEffect, useState } from 'react';
import { Alert, App, Button, Tag } from 'antd';
import Input from '../components/IMEInput';
import { FolderOpenOutlined, SendOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import AskFilePicker from '../components/AskFilePicker';
import { askFiles, getFileAI } from '../services/ai';
import { GraphicEmpty, PageHeader, ProductPage } from '../components/design/ProductUI';

const Workspace = styled.div`width: 100%; margin: 0;`;
const Composer = styled.section`
  display: grid;
  gap: 10px;

  .ant-input { min-height: 104px; padding: 14px 16px; resize: vertical; line-height: 1.7; }
`;
const ComposerActions = styled.div`display: flex; flex-wrap: wrap; justify-content: flex-end; align-items: center; gap: 8px;`;
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
  const { message } = App.useApp();
  const [question, setQuestion] = useState('');
  const [answer, setAnswer] = useState(null);
  const [asking, setAsking] = useState(false);
  const [selected, setSelected] = useState([]);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [invalidFiles, setInvalidFiles] = useState([]);
  const [scopeError, setScopeError] = useState('');
  const [preparing, setPreparing] = useState(false);
  useEffect(() => {
    const fileId = new URLSearchParams(window.location.search).get('file');
    if (!fileId) return undefined;
    const controller = new AbortController();
    let timer;
    let attempts = 0;
    setPreparing(true);
    const check = async () => {
      try {
        const info = await getFileAI(fileId, { signal: controller.signal });
        if (controller.signal.aborted) return;
        if (info.capabilities?.askable) {
          setSelected([{ id: fileId, name: new URLSearchParams(window.location.search).get('name') || '指定文件' }]);
          setPreparing(false); setScopeError('');
        } else if (++attempts < 60 && info.capabilities?.text?.latest_build?.state !== 'failed') {
          timer = setTimeout(check, 2000);
        } else { setPreparing(false); setScopeError('这个文件尚未准备好问答，请在文件菜单中重建后重试。'); }
      } catch (error) { if (!controller.signal.aborted) { setPreparing(false); setScopeError(error.message || '无法读取指定文件'); } }
    };
    check();
    return () => { controller.abort(); clearTimeout(timer); };
  }, []);

  const runAsk = async () => {
    const value = question.trim();
    if (!value || asking || preparing || scopeError) return;
    const scope = selected.length ? { mode: 'selected', file_ids: selected.map(file => file.id) } : { mode: 'all' };
    setAsking(true);
    setQuestion('');
    setAnswer(null);
    try {
      setAnswer(await askFiles(value, { scope }));
    } catch (error) {
      if (error.code === 'ai.scope_invalid' || error.code === 'ai.scope_changed') {
        setQuestion(current => current || value);
        setScopeError(error.message || '选择已失效，请更新文件范围');
        setInvalidFiles(error.invalidFiles || []);
      } else message.error(error.message || '问答失败');
    } finally {
      setAsking(false);
    }
  };

  return <ProductPage accent="var(--mint)">
    <Workspace>
      <PageHeader title="问答" />
      <Composer className="page-reveal">
        {preparing && <Alert type="info" message="正在准备指定文件的问答文本…" />}
        {scopeError && <Alert type="warning" message={scopeError} description={invalidFiles.length ? invalidFiles.map(item => <Tag key={item.file_id}>{selected.find(file => file.id === item.file_id)?.name || '所选文件'}：{item.reason === 'text_unavailable' ? '文本尚未准备好' : '已不可用'}</Tag>) : null} />}

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
        {answer?.retrieval?.degraded && <Alert type="info" message="本次使用可用的文本检索生成回答" />}
        {answer?.coverage?.missing_evidence_file_ids?.length > 0 && <Alert type="warning" message="部分所选文件没有找到相关证据，回答未覆盖这些文件。" />}

        {answer
          ? <Answer className="page-reveal">{answer.answer}</Answer>
          : <GraphicEmpty title={asking ? '正在整理答案' : '等待你的问题'} />}
      </ResultSection>
      <AskFilePicker open={pickerOpen} selected={selected} onCancel={() => setPickerOpen(false)} onApply={files => { setSelected(files); setScopeError(''); setInvalidFiles([]); setPickerOpen(false); }} />
    </Workspace>
  </ProductPage>;
};

export default AIAsk;
