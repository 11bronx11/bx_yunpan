import React, { useState } from 'react';
import { App, Button, Input, Tag } from 'antd';
import { CommentOutlined, SendOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import { askFiles } from '../services/ai';
import { GraphicEmpty, PageHeader, ProductPage } from '../components/design/ProductUI';

const Workspace = styled.div`width: 100%; margin: 0;`;
const Composer = styled.section`
  display: grid;
  gap: 10px;

  .ant-input { min-height: 104px; padding: 14px 16px; resize: vertical; line-height: 1.7; }
`;
const ComposerActions = styled.div`display: flex; justify-content: flex-end;`;
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
`;
const Citations = styled.div`display: flex; flex-wrap: wrap; gap: 8px; margin-top: 18px;`;

const AIAsk = () => {
  const { message } = App.useApp();
  const [question, setQuestion] = useState('');
  const [answer, setAnswer] = useState(null);
  const [asking, setAsking] = useState(false);

  const runAsk = async () => {
    const value = question.trim();
    if (!value || asking) return;
    setAsking(true);
    setQuestion('');
    setAnswer(null);
    try {
      setAnswer(await askFiles(value));
    } catch (error) {
      message.error(error.message || '问答失败');
    } finally {
      setAsking(false);
    }
  };

  return <ProductPage accent="var(--mint)">
    <Workspace>
      <PageHeader title="AI 问答" description="从已索引的网盘文件中整理答案。" />
      <Composer className="page-reveal">
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
        <ComposerActions><Button aria-label="提问" type="primary" icon={<SendOutlined />} loading={asking} disabled={!question.trim()} onClick={runAsk}>提问</Button></ComposerActions>
      </Composer>
      <ResultSection>
        <ResultTitle>回答</ResultTitle>
        {answer
          ? <Answer className="page-reveal">{answer.answer}<Citations>{answer.citations?.map(citation => <Tag key={citation.id} color="green"><CommentOutlined /> {citation.file_name}{citation.page_number ? ` / 第 ${citation.page_number} 页` : ''}</Tag>)}</Citations></Answer>
          : <GraphicEmpty title={asking ? '正在整理答案' : '等待你的问题'} description="已完成 AI 索引的文件会参与回答。" />}
      </ResultSection>
    </Workspace>
  </ProductPage>;
};

export default AIAsk;
