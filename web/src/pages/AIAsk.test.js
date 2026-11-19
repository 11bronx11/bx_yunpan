import React from 'react';
import { App } from 'antd';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import AIAsk from './AIAsk';
import { askFiles, getFileAI, listAskableFiles } from '../services/ai';
import { getRoot, getChildren } from '../services/drive';

jest.mock('../services/drive', () => ({ getRoot: jest.fn(), getChildren: jest.fn() }));
jest.mock('../services/ai', () => ({ askFiles: jest.fn(), listAskableFiles: jest.fn(), getFileAI: jest.fn() }));
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
  render(<App><AIAsk /></App>);

  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '有哪些待跟进事项？' } });
  await waitFor(() => expect(screen.getByRole('button', { name: '提问' })).toBeVisible());
  fireEvent.click(screen.getByRole('button', { name: '提问' }));

  await waitFor(() => expect(askFiles).toHaveBeenCalledWith('有哪些待跟进事项？', { scope: { mode: 'all' } }));
  await waitFor(() => expect(screen.getByLabelText('向文件提问')).toHaveValue(''));
  expect(await screen.findByText('待跟进事项包括接口联调和上线检查。')).toBeInTheDocument();
  expect(screen.queryByText(/项目复盘\.pdf/)).not.toBeInTheDocument();
  expect(screen.queryByText(/本次范围/)).not.toBeInTheDocument();
  expect(screen.queryByLabelText('本次问答范围')).not.toBeInTheDocument();
});

test('confirms IME text without asking, keeps Shift+Enter, and submits with a later Enter', async () => {
  askFiles.mockResolvedValue({ answer: '回答', citations: [] });
  render(<App><AIAsk /></App>);
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
  expect(askFiles).toHaveBeenCalledWith('hello', { scope: { mode: 'all' } });
  expect(input).toHaveValue('');
});

test('keeps the submitted question cleared when the request fails', async () => {
  askFiles.mockRejectedValue(new Error('网络连接失败'));
  render(<App><AIAsk /></App>);
  const input = screen.getByLabelText('向文件提问');
  fireEvent.change(input, { target: { value: '需要重试的问题' } });
  fireEvent.click(await screen.findByRole('button', { name: '提问' }));
  await screen.findByText('网络连接失败');
  expect(input).toHaveValue('');
});

test('does not overwrite a new draft when the previous question fails', async () => {
  let rejectAsk;
  askFiles.mockImplementationOnce(() => new Promise((resolve, reject) => { rejectAsk = reject; }));
  render(<App><AIAsk /></App>);
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
  render(<App><AIAsk /></App>);
  await waitFor(() => expect(screen.queryByText('正在准备指定文件的问答文本…')).not.toBeInTheDocument());
  expect(screen.queryByText('alpha.txt')).not.toBeInTheDocument();
  const input = screen.getByLabelText('向文件提问');
  fireEvent.change(input, { target: { value: '保留这个问题' } });
  fireEvent.click(screen.getByRole('button', { name: '提问' }));
  await screen.findByText('选择已失效');
  expect(input).toHaveValue('保留这个问题');
  expect(askFiles).toHaveBeenCalledTimes(1);
  expect(askFiles).toHaveBeenCalledWith('保留这个问题', { scope: { mode: 'selected', file_ids: ['selected-id'] } });
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
  expect(askFiles).toHaveBeenLastCalledWith('保留这个问题', { scope: { mode: 'all' } });
});
