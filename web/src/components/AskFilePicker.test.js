import React from 'react';
import { App } from 'antd';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import AskFilePicker from './AskFilePicker';
import { listAskableFiles } from '../services/ai';
import { getRoot, getChildren } from '../services/drive';

jest.mock('../services/ai', () => ({ listAskableFiles: jest.fn() }));
jest.mock('../services/drive', () => ({ getRoot: jest.fn(), getChildren: jest.fn() }));
jest.setTimeout(60000);
const alpha = { id: 'a', name: 'alpha.txt', path: '/docs/alpha.txt' };
const beta = { id: 'b', name: 'beta.txt', path: '/docs/beta.txt' };
beforeEach(() => {
  jest.clearAllMocks();
  getRoot.mockResolvedValue({ id: 'root' });
  getChildren.mockResolvedValue({ items: [], page: { has_more: false } });
});

test('preserves a draft across pages and server search, and applies both files', async () => {
  listAskableFiles.mockImplementation(({ cursor, q }) => Promise.resolve(q ? { items: [beta] } : cursor ? { items: [beta] } : { items: [alpha], page: { has_more: true, next_cursor: 'next' } }));
  const apply = jest.fn();
  render(<AskFilePicker open selected={[]} onApply={apply} onCancel={jest.fn()} />);
  await screen.findByRole('checkbox', { name: /alpha.txt/ });
  expect(listAskableFiles).toHaveBeenCalledWith(expect.objectContaining({ q: '', folder_id: '', include_subfolders: true }));
  fireEvent.click(screen.getByText('alpha.txt'));
  fireEvent.click(screen.getByRole('button', { name: '加载更多' }));
  fireEvent.click(await screen.findByRole('checkbox', { name: /beta.txt/ }));
  fireEvent.change(screen.getByLabelText('搜索可问答文件'), { target: { value: 'beta' } });
  await waitFor(() => expect(listAskableFiles).toHaveBeenCalledWith(expect.objectContaining({ q: 'beta', folder_id: '', include_subfolders: true })));
  await waitFor(() => expect(screen.getByLabelText('可问答文件')).toHaveAttribute('aria-busy', 'false'));
  expect(screen.getByRole('checkbox', { name: /beta.txt/ })).toBeChecked();
  fireEvent.click(screen.getByRole('button', { name: '应用选择' }));
  expect(apply).toHaveBeenCalledWith([alpha, beta]);
});

test('cancel discards changes when reopened and clearing does not silently apply', async () => {
  listAskableFiles.mockResolvedValue({ items: [alpha, beta] });
  const apply = jest.fn(), cancel = jest.fn(), selected = [alpha];
  const view = render(<AskFilePicker open selected={selected} onApply={apply} onCancel={cancel} />);
  fireEvent.click(await screen.findByRole('checkbox', { name: /beta.txt/ }));
  fireEvent.click(screen.getByRole('button', { name: /取\s*消/ }));
  expect(cancel).toHaveBeenCalledTimes(1);
  expect(apply).not.toHaveBeenCalled();
  view.rerender(<AskFilePicker open={false} selected={selected} onApply={apply} onCancel={cancel} />);
  view.rerender(<AskFilePicker open selected={selected} onApply={apply} onCancel={cancel} />);
  await waitFor(() => expect(screen.getByRole('checkbox', { name: /beta.txt/ })).not.toBeChecked());
  expect(screen.getByRole('checkbox', { name: /alpha.txt/ })).toBeChecked();
  fireEvent.click(screen.getByRole('button', { name: '清空选择' }));
  expect(screen.getByRole('button', { name: '应用选择' })).toBeEnabled();
  expect(apply).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: '应用选择' }));
  expect(apply).toHaveBeenCalledWith([]);
});

test('enforces 50 files while allowing deselection and continuing an empty scan page', async () => {
  const selected = Array.from({ length: 50 }, (_, n) => ({ id: String(n), name: `selected-${n}.txt` }));
  listAskableFiles.mockImplementation(({ cursor }) => Promise.resolve(cursor ? { items: [selected[0], beta] } : { items: [], page: { has_more: true, next_cursor: 'sparse' }, scan_incomplete: true }));
  render(<AskFilePicker open selected={selected} onApply={jest.fn()} onCancel={jest.fn()} />);
  fireEvent.click(await screen.findByRole('button', { name: '加载更多' }));
  expect(await screen.findByRole('checkbox', { name: /beta.txt/ })).toBeDisabled();
  fireEvent.click(screen.getByRole('checkbox', { name: /selected-0.txt/ }));
  expect(screen.getByRole('checkbox', { name: /beta.txt/ })).not.toBeDisabled();
  fireEvent.click(screen.getByRole('checkbox', { name: /beta.txt/ }));
  expect(screen.getByText('最多选择 50 个文件')).toBeVisible();
});

test('loads nested directory filters from the real items and page contract', async () => {
  listAskableFiles.mockResolvedValue({ items: [alpha] });
  getChildren.mockImplementation((id, { cursor }) => Promise.resolve(cursor
    ? { items: [{ id: 'nested', name: '项目文档', type: 'folder' }], page: { has_more: false } }
    : { items: [{ id: 'file', name: '不是目录.txt', type: 'file' }], page: { has_more: true, next_cursor: 'directories' } }));
  render(<AskFilePicker open selected={[]} onApply={jest.fn()} onCancel={jest.fn()} />);
  const directory = screen.getByRole('combobox', { name: '按目录筛选' });
  fireEvent.keyDown(directory, { key: 'ArrowDown', keyCode: 40, which: 40 });
  await screen.findByText('根目录');
  fireEvent.click(await screen.findByText('项目文档'));
  await waitFor(() => expect(listAskableFiles).toHaveBeenCalledWith(expect.objectContaining({ folder_id: 'nested', include_subfolders: false })));
  expect(screen.queryByText('/docs/alpha.txt')).not.toBeInTheDocument();
  expect(screen.queryByRole('combobox', { name: '文件类型' })).not.toBeInTheDocument();
  expect(getChildren).toHaveBeenCalledWith('root', expect.objectContaining({ cursor: 'directories' }));
  expect(screen.queryByText('不是目录.txt')).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText('搜索可问答文件'), { target: { value: 'alpha' } });
  await waitFor(() => expect(listAskableFiles).toHaveBeenLastCalledWith(expect.objectContaining({ q: 'alpha', folder_id: '', include_subfolders: true })));
  await waitFor(() => expect(screen.getByLabelText('可问答文件')).toHaveAttribute('aria-busy', 'false'));
});

test('keeps loading rows inactive, ignores an old directory response, and reuses visited directories', async () => {
  let resolveSlow;
  getChildren.mockResolvedValue({ items: [{ id: 'slow', name: '慢目录', type: 'folder' }, { id: 'fast', name: '快目录', type: 'folder' }] });
  listAskableFiles.mockImplementation(({ folder_id }) => folder_id === 'slow'
    ? new Promise(resolve => { resolveSlow = resolve; })
    : Promise.resolve({ items: folder_id === 'fast' ? [beta] : [alpha] }));
  render(<AskFilePicker open selected={[]} onApply={jest.fn()} onCancel={jest.fn()} />);
  await screen.findByRole('checkbox', { name: /alpha.txt/ });
  const directory = screen.getByRole('combobox', { name: '按目录筛选' });
  const waitForIdle = () => waitFor(() => expect(screen.getByLabelText('可问答文件')).toHaveAttribute('aria-busy', 'false'));
  const choose = async (name, waitForFiles = true) => {
    fireEvent.keyDown(directory, { key: 'ArrowDown', keyCode: 40, which: 40 });
    const item = await screen.findByText(name, { selector: '.ant-select-tree-title' });
    fireEvent.click(item);
    if (waitForFiles) await waitForIdle();
  };
  await choose('慢目录', false);
  expect(listAskableFiles).toHaveBeenLastCalledWith(expect.objectContaining({ folder_id: 'slow', include_subfolders: false }));
  expect(screen.getByRole('checkbox', { name: /alpha.txt/ })).toBeDisabled();
  expect(screen.getByLabelText('可问答文件')).toHaveAttribute('aria-busy', 'true');
  await choose('快目录');
  expect(await screen.findByRole('checkbox', { name: /beta.txt/ })).toBeEnabled();
  expect(screen.queryByText('/docs/beta.txt')).not.toBeInTheDocument();
  await act(async () => resolveSlow({ items: [{ id: 'late', name: '过期结果.txt' }] }));
  expect(screen.queryByText('过期结果.txt')).not.toBeInTheDocument();
  await choose('根目录');
  await screen.findByRole('checkbox', { name: /alpha.txt/ });
  const callsBeforeReturn = listAskableFiles.mock.calls.length;
  await choose('快目录');
  expect(screen.getByRole('checkbox', { name: /beta.txt/ })).toBeEnabled();
  expect(listAskableFiles).toHaveBeenCalledTimes(callsBeforeReturn);
});

test('shows a file list failure as a top message and lets the user retry', async () => {
  listAskableFiles.mockRejectedValueOnce(new Error('功能暂时不可用')).mockResolvedValue({ items: [alpha] });
  render(<App><AskFilePicker open selected={[]} onApply={jest.fn()} onCancel={jest.fn()} /></App>);
  expect(await screen.findByText('功能暂时不可用', { selector: '.ant-message-error span' })).toBeVisible();
  expect(screen.queryByText('功能暂时不可用', { selector: '.ant-alert-message' })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /重\s*试/ }));
  expect(await screen.findByRole('checkbox', { name: /alpha.txt/ })).toBeEnabled();
});

test.each(['root', 'children'])('shows a %s directory failure once as a top message', async source => {
  listAskableFiles.mockResolvedValue({ items: [alpha] });
  if (source === 'root') getRoot.mockRejectedValueOnce(new Error('目录加载失败'));
  else getChildren.mockRejectedValueOnce(new Error('目录加载失败'));
  render(<App><AskFilePicker open selected={[]} onApply={jest.fn()} onCancel={jest.fn()} /></App>);
  const text = '目录暂时无法加载，请重新打开选择文件。';
  expect(await screen.findByText(text, { selector: '.ant-message-error span' })).toBeVisible();
  expect(screen.getAllByText(text)).toHaveLength(1);
  expect(screen.queryByText(text, { selector: '.ant-alert-message' })).not.toBeInTheDocument();
  expect(await screen.findByRole('checkbox', { name: /alpha.txt/ })).toBeEnabled();
});
