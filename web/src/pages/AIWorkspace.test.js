import React from 'react';
import { App } from 'antd';
import { act, fireEvent, render, screen } from '@testing-library/react';
import AIWorkspace from './AIWorkspace';
import { searchFiles } from '../services/ai';

jest.mock('../services/ai', () => ({ askFiles: jest.fn(), searchFiles: jest.fn() }));
jest.mock('../components/SearchResultModal', () => ({ hit, open }) => open ? <div role="dialog">已打开 {hit.file.name}</div> : null);
beforeEach(() => jest.clearAllMocks());

test('opens a search result with the keyboard', async () => {
  searchFiles.mockResolvedValue({
    hits: [{
      file: { id: 'file-1', name: 'report.pdf', version: 1 },
      match_type: 'hybrid',
      score: 0.016,
      citations: [],
    }],
  });
  render(<App><AIWorkspace /></App>);

  fireEvent.change(screen.getByPlaceholderText('例如：上季度项目复盘中的风险'), { target: { value: '项目风险' } });
  fireEvent.click(screen.getByRole('button', { name: /搜索$/ }));
  const result = await screen.findByRole('button', { name: '打开 report.pdf 文件详情' });
  fireEvent.keyDown(result, { key: 'Enter' });

  expect(screen.getByRole('dialog')).toHaveTextContent('已打开 report.pdf');
});

test('waits for IME confirmation before allowing Enter to search', async () => {
  searchFiles.mockResolvedValue({ hits: [] });
  render(<App><AIWorkspace /></App>);
  const input = screen.getByPlaceholderText('例如：上季度项目复盘中的风险');
  fireEvent.compositionStart(input);
  fireEvent.change(input, { target: { value: 'hello' } });
  fireEvent.keyDown(input, { key: 'Enter', isComposing: true });
  fireEvent.keyUp(input, { key: 'Enter' });
  expect(searchFiles).not.toHaveBeenCalled();
  fireEvent.compositionEnd(input, { data: 'hello' });
  fireEvent.keyDown(input, { key: 'Enter' });
  expect(searchFiles).toHaveBeenCalledTimes(1);
  expect(searchFiles).toHaveBeenCalledWith('hello', 'hybrid', { signal: expect.any(AbortSignal) });
  expect(input).toHaveValue('hello');
  await screen.findByText('0 个文件');
});

test('keeps the latest search result when an earlier request finishes later', async () => {
  let resolveOld;
  searchFiles.mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve; }))
    .mockResolvedValueOnce({ hits: [{ file: { id: 'new', name: 'new.txt' }, citations: [] }] });
  render(<App><AIWorkspace /></App>);
  const input = screen.getByPlaceholderText('例如：上季度项目复盘中的风险');
  fireEvent.change(input, { target: { value: 'old' } });
  fireEvent.keyDown(input, { key: 'Enter' });
  fireEvent.keyUp(input, { key: 'Enter' });
  fireEvent.change(input, { target: { value: 'new' } });
  fireEvent.keyDown(input, { key: 'Enter' });
  fireEvent.keyUp(input, { key: 'Enter' });
  expect(await screen.findByText('new.txt')).toBeInTheDocument();
  await act(async () => resolveOld({ hits: [{ file: { id: 'old', name: 'old.txt' }, citations: [] }] }));
  expect(screen.queryByText('old.txt')).not.toBeInTheDocument();
  expect(screen.getByText('new.txt')).toBeInTheDocument();
  expect(searchFiles.mock.calls[0][2].signal.aborted).toBe(true);
});

test('leaving the search page aborts its pending request', async () => {
  searchFiles.mockImplementationOnce(() => new Promise(() => {}));
  const view = render(<App><AIWorkspace /></App>);
  const input = screen.getByPlaceholderText('例如：上季度项目复盘中的风险');
  fireEvent.change(input, { target: { value: 'pending' } });
  fireEvent.keyDown(input, { key: 'Enter' });
  const signal = searchFiles.mock.calls[0][2].signal;
  view.unmount();
  expect(signal.aborted).toBe(true);
});
