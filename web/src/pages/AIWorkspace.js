import React, { useState } from 'react';
import { App, Button, Input, Select, Space, Tag } from 'antd';
import { FileSearchOutlined, SearchOutlined } from '@ant-design/icons';
import { searchFiles } from '../services/ai';
import { GraphicEmpty, PageHeader, ProductPage, SectionHeading, StatusTag } from '../components/design/ProductUI';
import SearchResultModal from '../components/SearchResultModal';
import styled from '@emotion/styled';

const SearchBar = styled.div`display: grid; grid-template-columns: minmax(0, 1fr) 150px auto; gap: 10px; margin-bottom: 24px; @media (max-width: 680px) { grid-template-columns: 1fr; }`;
const ResultList = styled.div`display: grid; gap: 12px;`;
const Result = styled.article`
  display: grid;
  grid-template-columns: minmax(0, 1fr) 120px;
  gap: 18px;
  padding: 18px;
  border: 1px solid var(--divider);
  border-radius: var(--radius-control);
  background: var(--white);
  cursor: pointer;
  transition: border-color var(--transition-fast), background var(--transition-fast), transform var(--transition-fast);

  &:hover { border-color: var(--divider-strong); background: var(--surface-hover); transform: translateY(-1px); }
  &:focus-visible { outline: 2px solid var(--blue); outline-offset: 2px; }
  @media (prefers-reduced-motion: reduce) { transition: none; }
  @media (max-width: 600px) { grid-template-columns: 1fr; }
`;
const Excerpt = styled.p`margin: 9px 0 0; color: var(--muted); font-size: 13px; line-height: 1.6;`;

const AIWorkspace = () => {
  const { message } = App.useApp();
  const [query, setQuery] = useState('');
  const [mode, setMode] = useState('hybrid');
  const [results, setResults] = useState([]);
  const [searching, setSearching] = useState(false);
  const [selectedHit, setSelectedHit] = useState(null);
  const runSearch = async () => {
    if (!query.trim()) return;
    setSearching(true);
    try { const response = await searchFiles(query, mode); setResults(response.hits || []); } catch (error) { message.error(error.message || '搜索失败'); } finally { setSearching(false); }
  };
  const openResult = hit => setSelectedHit(hit);
  const handleResultKeyDown = (event, hit) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    openResult(hit);
  };
  return <ProductPage accent="var(--blue)">
    <PageHeader title="AI 智能检索" count="FTS · 向量 · RRF" description="在你有权限的文件中按名称、全文和语义查找，并查看可追溯引用。" />
    <SearchBar><Input size="large" prefix={<SearchOutlined />} value={query} onChange={event => setQuery(event.target.value)} onPressEnter={runSearch} placeholder="例如：上季度项目复盘中的风险" /><Select size="large" value={mode} onChange={setMode} options={[{ value: 'hybrid', label: '混合检索' }, { value: 'name', label: '文件名' }, { value: 'fulltext', label: '全文' }, { value: 'semantic', label: '语义' }]} /><Button type="primary" size="large" icon={<FileSearchOutlined />} loading={searching} onClick={runSearch}>搜索</Button></SearchBar>
    <SectionHeading title="检索结果" meta={`${results.length} 个文件`} />
    {results.length === 0 ? <GraphicEmpty title="输入一句话开始检索" description="索引在 Worker 中异步构建，引用会保留文件、页码和段落。" /> : <ResultList>{results.map(hit => <Result key={hit.file.id} role="button" tabIndex={0} aria-label={`打开 ${hit.file.name} 文件详情`} onClick={() => openResult(hit)} onKeyDown={event => handleResultKeyDown(event, hit)}><div><Space><strong>{hit.file.name}</strong><StatusTag tone="blue">{hit.match_type}</StatusTag></Space>{hit.citations?.map(citation => <Excerpt key={citation.id}>“{citation.excerpt}” <Tag color="blue">{citation.page_number ? `第 ${citation.page_number} 页` : citation.section || '正文'}</Tag></Excerpt>)}</div><div><strong>{(hit.score || 0).toFixed(3)}</strong><small> relevance</small></div></Result>)}</ResultList>}
    <SearchResultModal hit={selectedHit} open={Boolean(selectedHit)} onClose={() => setSelectedHit(null)} onChanged={runSearch} />
  </ProductPage>;
};

export default AIWorkspace;
