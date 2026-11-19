import React from 'react';
import { App } from 'antd';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { Link, MemoryRouter, Route, Routes } from 'react-router-dom';
import AIAsk from './AIAsk';
import { AskProvider } from '../contexts/AskContext';
import { askFiles, getFileAI, listAskableFiles } from '../services/ai';
import { getRoot, getChildren } from '../services/drive';

jest.mock('../services/drive', () => ({ getRoot: jest.fn(), getChildren: jest.fn() }));
jest.mock('../services/ai', () => ({ askFiles: jest.fn(), listAskableFiles: jest.fn(), getFileAI: jest.fn() }));
const AskWorkspace = ({ owner = 'user-1' }) => <App><AskProvider key={owner}>
  <MemoryRouter initialEntries={[window.location.pathname + window.location.search]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
    <nav><Link to="/shares">离开问答</Link><Link to="/ask">返回问答</Link></nav>
    <Routes><Route path="/ask" element={<AIAsk />} /><Route path="/shares" element={<h1>其他页面</h1>} /></Routes>
  </MemoryRouter>
</AskProvider></App>;
const renderAsk = () => render(<AskWorkspace />);

beforeEach(() => {
  jest.clearAllMocks();
  getRoot.mockResolvedValue({ id: 'root' });
  getChildren.mockResolvedValue({ items: [], page: { has_more: false } });
  window.history.replaceState({}, '', '/ask');
});

test('asks all indexed files and renders the answer without scope or evidence slices', async () => {
  askFiles.mockResolvedValue({
    answer: '待跟进事项包括接口联调和上线检查。',
    citations: [{ id: 'citation-1', file_name: '项目复盘.pdf', page_number: 3 }],
  });
  renderAsk();

  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '有哪些待跟进事项？' } });
  await waitFor(() => expect(screen.getByRole('button', { name: '提问' })).toBeVisible());
  fireEvent.click(screen.getByRole('button', { name: '提问' }));

  await waitFor(() => expect(askFiles).toHaveBeenCalledWith('有哪些待跟进事项？', expect.objectContaining({ scope: { mode: 'all' }, signal: expect.any(AbortSignal) })));
  await waitFor(() => expect(screen.getByLabelText('向文件提问')).toHaveValue(''));
  expect(await screen.findByText('待跟进事项包括接口联调和上线检查。')).toBeInTheDocument();
  expect(screen.queryByText(/项目复盘\.pdf/)).not.toBeInTheDocument();
  expect(screen.queryByText(/本次范围/)).not.toBeInTheDocument();
  expect(screen.queryByLabelText('本次问答范围')).not.toBeInTheDocument();
});

test('confirms IME text without asking, keeps Shift+Enter, and submits with a later Enter', async () => {
  askFiles.mockResolvedValue({ answer: '回答', citations: [] });
  renderAsk();
  const input = screen.getByLabelText('向文件提问');
  fireEvent.compositionStart(input);
  fireEvent.change(input, { target: { value: 'hello' } });
  expect(fireEvent.keyDown(input, { key: 'Enter', isComposing: true })).toBe(true);
  fireEvent.keyUp(input, { key: 'Enter' });
  expect(askFiles).not.toHaveBeenCalled();
  expect(input).toHaveValue('hello');
  fireEvent.compositionEnd(input, { data: 'hello' });
  expect(fireEvent.keyDown(input, { key: 'Enter', shiftKey: true })).toBe(true);
  fireEvent.keyUp(input, { key: 'Enter' });
  expect(askFiles).not.toHaveBeenCalled();
  fireEvent.keyDown(input, { key: 'Enter' });
  await waitFor(() => expect(askFiles).toHaveBeenCalledTimes(1));
  expect(askFiles).toHaveBeenCalledWith('hello', expect.objectContaining({ scope: { mode: 'all' } }));
  expect(input).toHaveValue('');
});

test('keeps the submitted question cleared when the request fails', async () => {
  askFiles.mockRejectedValue(new Error('网络连接失败'));
  renderAsk();
  const input = screen.getByLabelText('向文件提问');
  fireEvent.change(input, { target: { value: '需要重试的问题' } });
  fireEvent.click(await screen.findByRole('button', { name: '提问' }));
  await screen.findByText('网络连接失败');
  expect(input).toHaveValue('');
});

test('does not overwrite a new draft when the previous question fails', async () => {
  let rejectAsk;
  askFiles.mockImplementationOnce(() => new Promise((resolve, reject) => { rejectAsk = reject; }));
  renderAsk();
  const input = screen.getByLabelText('向文件提问');
  fireEvent.change(input, { target: { value: '旧问题' } });
  fireEvent.click(await screen.findByRole('button', { name: '提问' }));
  fireEvent.change(input, { target: { value: '新的草稿' } });
  await act(async () => rejectAsk(new Error('网络连接失败')));
  expect(input).toHaveValue('新的草稿');
});

test('keeps an invalid selected scope and question without retrying all files', async () => {
  window.history.replaceState({}, '', '/ask?file=selected-id&name=alpha.txt');
  getFileAI.mockResolvedValue({ capabilities: { askable: true } });
  listAskableFiles.mockResolvedValue({ items: [{ id: 'selected-id', name: 'alpha.txt' }] });
  askFiles.mockRejectedValue(Object.assign(new Error('选择已失效'), { code: 'ai.scope_invalid' }));
  renderAsk();
  await waitFor(() => expect(screen.queryByText('正在准备指定文件的问答文本…')).not.toBeInTheDocument());
  expect(screen.queryByText('alpha.txt')).not.toBeInTheDocument();
  const input = screen.getByLabelText('向文件提问');
  fireEvent.change(input, { target: { value: '保留这个问题' } });
  fireEvent.click(screen.getByRole('button', { name: '提问' }));
  await screen.findByText('选择已失效');
  expect(input).toHaveValue('保留这个问题');
  expect(askFiles).toHaveBeenCalledTimes(1);
  expect(askFiles).toHaveBeenCalledWith('保留这个问题', expect.objectContaining({ scope: { mode: 'selected', file_ids: ['selected-id'] } }));
  expect(screen.getByRole('button', { name: '提问' })).toBeDisabled();
  fireEvent.click(screen.getByRole('button', { name: '选择文件' }));
  expect(await screen.findByRole('checkbox', { name: /alpha.txt/ })).toBeChecked();
  fireEvent.click(screen.getByRole('button', { name: '清空选择' }));
  expect(screen.getByRole('button', { name: '提问' })).toBeDisabled();
  fireEvent.click(screen.getByRole('button', { name: '应用选择' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  expect(screen.getByRole('button', { name: '提问' })).toBeEnabled();
  expect(input).toHaveValue('保留这个问题');
  askFiles.mockResolvedValue({ answer: '全部文件的回答' });
  fireEvent.click(screen.getByRole('button', { name: '提问' }));
  await screen.findByText('全部文件的回答');
  expect(askFiles).toHaveBeenLastCalledWith('保留这个问题', expect.objectContaining({ scope: { mode: 'all' } }));
});

test('keeps a pending answer, selection and next draft across route changes without resubmitting', async () => {
  window.history.replaceState({}, '', '/ask?file=selected-id&name=alpha.txt');
  getFileAI.mockResolvedValue({ capabilities: { askable: true } });
  let finish;
  askFiles.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
  renderAsk();
  await waitFor(() => expect(screen.getByLabelText('当前选择文件数量')).toHaveTextContent('已选 1 / 50'));
  const input = screen.getByLabelText('向文件提问');
  fireEvent.change(input, { target: { value: '提交的问题' } });
  fireEvent.click(screen.getByRole('button', { name: '提问' }));
  fireEvent.change(input, { target: { value: '下一问的草稿' } });
  fireEvent.click(screen.getByRole('link', { name: '离开问答' }));
  await screen.findByRole('heading', { name: '其他页面' });
  expect(askFiles.mock.calls[0][1].signal.aborted).toBe(false);
  fireEvent.click(screen.getByRole('link', { name: '返回问答' }));
  expect(await screen.findByText('正在整理答案')).toBeInTheDocument();
  expect(screen.getByLabelText('向文件提问')).toHaveValue('下一问的草稿');
  expect(screen.getByLabelText('当前选择文件数量')).toHaveTextContent('已选 1 / 50');
  fireEvent.keyDown(screen.getByLabelText('向文件提问'), { key: 'Enter' });
  expect(askFiles).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole('link', { name: '离开问答' }));
  await act(async () => finish({ answer: '离开期间完成的回答' }));
  expect(screen.queryByText('离开期间完成的回答')).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('link', { name: '返回问答' }));
  expect(await screen.findByText('离开期间完成的回答')).toBeInTheDocument();
  expect(screen.getByLabelText('向文件提问')).toHaveValue('下一问的草稿');
  expect(screen.getByLabelText('当前选择文件数量')).toHaveTextContent('已选 1 / 50');
  expect(getFileAI).toHaveBeenCalledTimes(1);
  expect(askFiles).toHaveBeenCalledTimes(1);
});

test.each([
  { code: undefined, text: '网络连接失败', restoredQuestion: '', blocksRetry: false },
  { code: 'ai.scope_invalid', text: '选择已失效', restoredQuestion: '原问题', blocksRetry: true },
])('retains a background failure on return: $text', async ({ code, text, restoredQuestion, blocksRetry }) => {
  let fail;
  askFiles.mockImplementationOnce(() => new Promise((resolve, reject) => { fail = reject; }));
  renderAsk();
  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '原问题' } });
  fireEvent.click(screen.getByRole('button', { name: '提问' }));
  fireEvent.click(screen.getByRole('link', { name: '离开问答' }));
  await act(async () => fail(Object.assign(new Error(text), { code })));
  fireEvent.click(screen.getByRole('link', { name: '返回问答' }));
  expect(await screen.findByText(text)).toBeInTheDocument();
  expect(screen.getByLabelText('向文件提问')).toHaveValue(restoredQuestion);
  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '下一问' } });
  expect(screen.getByRole('button', { name: '提问' })).toHaveProperty('disabled', blocksRetry);
  expect(askFiles).toHaveBeenCalledTimes(1);
});

test('clears and aborts on pagehide, and ignores a late answer', async () => {
  window.history.replaceState({}, '', '/ask?file=selected-id&name=alpha.txt');
  getFileAI.mockResolvedValue({ capabilities: { askable: true } });
  let finish;
  askFiles.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
  renderAsk();
  await waitFor(() => expect(screen.getByLabelText('当前选择文件数量')).toHaveTextContent('已选 1 / 50'));
  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '刷新前的问题' } });
  fireEvent.click(screen.getByRole('button', { name: '提问' }));
  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '刷新前的草稿' } });
  act(() => { window.dispatchEvent(new Event('pagehide')); });
  expect(askFiles.mock.calls[0][1].signal.aborted).toBe(true);
  expect(screen.getByLabelText('向文件提问')).toHaveValue('');
  expect(screen.getByLabelText('当前选择文件数量')).toHaveTextContent('已选 0 / 50');
  expect(screen.getByText('等待你的问题')).toBeInTheDocument();
  await act(async () => finish({ answer: '刷新前的迟到回答' }));
  expect(screen.queryByText('刷新前的迟到回答')).not.toBeInTheDocument();
  expect(getFileAI).toHaveBeenCalledTimes(1);
});

test('aborts on session replacement and cannot expose the old answer to the new session', async () => {
  let finish;
  askFiles.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
  const view = renderAsk();
  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '旧账号的问题' } });
  fireEvent.click(screen.getByRole('button', { name: '提问' }));
  view.rerender(<AskWorkspace owner="user-2" />);
  expect(askFiles.mock.calls[0][1].signal.aborted).toBe(true);
  await act(async () => finish({ answer: '旧账号的回答' }));
  expect(screen.queryByText('旧账号的回答')).not.toBeInTheDocument();
  expect(screen.getByLabelText('向文件提问')).toHaveValue('');
  expect(screen.getByLabelText('当前选择文件数量')).toHaveTextContent('已选 0 / 50');
});

test('continues preparing a file shortcut while its page is unmounted', async () => {
  window.history.replaceState({}, '', '/ask?file=selected-id&name=alpha.txt');
  let finish;
  getFileAI.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
  renderAsk();
  expect(screen.getByText('正在准备指定文件的问答文本…')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('link', { name: '离开问答' }));
  expect(getFileAI.mock.calls[0][1].signal.aborted).toBe(false);
  await act(async () => finish({ capabilities: { askable: true } }));
  fireEvent.click(screen.getByRole('link', { name: '返回问答' }));
  await waitFor(() => expect(screen.getByLabelText('当前选择文件数量')).toHaveTextContent('已选 1 / 50'));
  expect(getFileAI).toHaveBeenCalledTimes(1);
});
