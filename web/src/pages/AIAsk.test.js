import React from 'react';
import { App } from 'antd';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import AIAsk from './AIAsk';
import { askFiles } from '../services/ai';

jest.mock('../services/ai', () => ({ askFiles: jest.fn() }));
beforeEach(() => jest.clearAllMocks());

test('asks indexed files and renders the answer with citations', async () => {
  askFiles.mockResolvedValue({
    answer: '待跟进事项包括接口联调和上线检查。',
    citations: [{ id: 'citation-1', file_name: '项目复盘.pdf', page_number: 3 }],
  });
  render(<App><AIAsk /></App>);

  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '有哪些待跟进事项？' } });
  await waitFor(() => expect(screen.getByRole('button', { name: '提问' })).toBeVisible());
  fireEvent.click(screen.getByRole('button', { name: '提问' }));

  await waitFor(() => expect(askFiles).toHaveBeenCalledWith('有哪些待跟进事项？'));
  await waitFor(() => expect(screen.getByLabelText('向文件提问')).toHaveValue(''));
  expect(await screen.findByText('待跟进事项包括接口联调和上线检查。')).toBeInTheDocument();
  expect(screen.getByText(/项目复盘\.pdf \/ 第 3 页/)).toBeInTheDocument();
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
  expect(askFiles).toHaveBeenCalledWith('hello');
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
