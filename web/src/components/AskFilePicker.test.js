import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import AskFilePicker from './AskFilePicker';
import { listAskableFiles } from '../services/ai';
import { getRoot, getChildren } from '../services/drive';

jest.mock('../services/ai', () => ({ listAskableFiles: jest.fn() }));
jest.mock('../services/drive', () => ({ getRoot: jest.fn(), getChildren: jest.fn() }));
jest.setTimeout(60000);
const alpha = { id: 'a', name: 'alpha.txt', path: '/docs/alpha.txt' };
const beta = { id: 'b', name: 'beta.txt', path: '/docs/beta.txt' };
beforeEach(() => { jest.clearAllMocks(); getRoot.mockResolvedValue({ id: 'root' }); });

test('preserves a draft across pages and server search, and applies both files', async () => {
  listAskableFiles.mockImplementation(({ cursor, q }) => Promise.resolve(q ? { items: [beta] } : cursor ? { items: [beta] } : { items: [alpha], page: { has_more: true, next_cursor: 'next' } }));
  const apply = jest.fn();
  render(<AskFilePicker open selected={[]} onApply={apply} onCancel={jest.fn()} />);
  fireEvent.click(await screen.findByRole('checkbox', { name: /alpha.txt/ }));
  fireEvent.click(screen.getByRole('button', { name: '加载更多' }));
  fireEvent.click(await screen.findByRole('checkbox', { name: /beta.txt/ }));
  fireEvent.change(screen.getByLabelText('搜索可问答文件'), { target: { value: 'beta' } });
  await waitFor(() => expect(listAskableFiles).toHaveBeenCalledWith(expect.objectContaining({ q: 'beta' })));
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
  fireEvent.click(screen.getByRole('button', { name: '清空草稿' }));
  expect(screen.getByRole('button', { name: '应用选择' })).toBeDisabled();
  expect(apply).not.toHaveBeenCalled();
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
  const view = render(<AskFilePicker open selected={[]} onApply={jest.fn()} onCancel={jest.fn()} />);
  fireEvent.mouseDown(screen.getByRole('combobox', { name: '按目录筛选' }).closest('.ant-select').querySelector('.ant-select-selector'));
  const root = await screen.findByText('根目录');
  fireEvent.click(root.closest('.ant-select-tree-treenode').querySelector('.ant-select-tree-switcher'));
  fireEvent.click(await screen.findByText('项目文档'));
  await waitFor(() => expect(listAskableFiles).toHaveBeenCalledWith(expect.objectContaining({ folder_id: 'nested' })));
  expect(getChildren).toHaveBeenCalledWith('root', expect.objectContaining({ cursor: 'directories' }));
  expect(view.container).not.toHaveTextContent('不是目录.txt');
});
