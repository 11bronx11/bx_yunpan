import React from 'react';
import { App as AntApp } from 'antd';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import MoveFileModal from './MoveFileModal';
import { getBreadcrumb, getChildren, moveFile } from '../services/drive';

jest.mock('../services/drive', () => ({
  getBreadcrumb: jest.fn(),
  getChildren: jest.fn(),
  moveFile: jest.fn(),
}));

beforeEach(() => {
  jest.clearAllMocks();
  getChildren.mockResolvedValue({
    items: [
      { id: 'current', type: 'folder', name: '当前目录' },
      { id: 'target', type: 'folder', name: '目标目录' },
      { id: 'file-2', type: 'file', name: 'ignored.txt' },
    ],
  });
  getBreadcrumb.mockResolvedValue({
    items: [{ id: 'root', name: '/' }, { id: 'target', name: '目标目录' }],
  });
  moveFile.mockResolvedValue({ id: 'file-1', folder_id: 'target', version: 4 });
});

test('moves a file to a selected directory and disables the current directory', async () => {
  const onMoved = jest.fn().mockResolvedValue(undefined);
  render(
    <AntApp>
      <MoveFileModal
        open
        file={{ id: 'file-1', name: 'report.pdf', version: 3 }}
        root={{ id: 'root', name: '/' }}
        currentFolderId="current"
        onCancel={jest.fn()}
        onMoved={onMoved}
      />
    </AntApp>,
  );

  expect(await screen.findByText('目标目录')).toBeInTheDocument();
  const moveButton = screen.getByRole('button', { name: /移\s*动/ });
  expect(moveButton).toBeDisabled();
  fireEvent.click(screen.getByRole('treeitem', { name: /当前位置，不可选择/ }));
  expect(moveButton).toBeDisabled();

  fireEvent.click(screen.getByText('目标目录'));
  expect(await screen.findByText('根目录 / 目标目录')).toBeInTheDocument();
  expect(moveButton).toBeEnabled();
  fireEvent.click(moveButton);

  await waitFor(() => expect(moveFile).toHaveBeenCalledWith('file-1', 3, 'target'));
  expect(onMoved).toHaveBeenCalledWith('根目录 / 目标目录');
});

const renderPicker = () => render(<AntApp><MoveFileModal open file={{ id: 'file-1', name: 'report.pdf', version: 3 }} root={{ id: 'root', name: '/' }} currentFolderId="current" onCancel={jest.fn()} onMoved={jest.fn()} /></AntApp>);

test('loads subsequent directories only when requested and stops at files', async () => {
  getChildren.mockResolvedValueOnce({ items: [{ id: 'first', type: 'folder', name: '第一批目录' }], page: { has_more: true, next_cursor: 'next' } })
    .mockResolvedValueOnce({ items: [{ id: 'last', type: 'folder', name: '最后目录' }, { id: 'ignored', type: 'file', name: 'ignored.txt' }], page: { has_more: true, next_cursor: 'files-only' } });
  const view = renderPicker();
  expect(await screen.findByText('第一批目录')).toBeInTheDocument();
  expect(getChildren).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole('button', { name: '加载更多' }));
  expect(await screen.findByText('最后目录')).toBeInTheDocument();
  expect(screen.getByText('第一批目录')).toBeInTheDocument();
  expect(screen.queryByText('ignored.txt')).not.toBeInTheDocument();
  expect(screen.queryByRole('button', { name: '加载更多' })).not.toBeInTheDocument();
  expect(getChildren).toHaveBeenNthCalledWith(2, 'root', expect.objectContaining({ cursor: 'next', limit: 50 }));
  const signal = getChildren.mock.calls[1][1].signal;
  view.unmount();
  expect(signal.aborted).toBe(true);
});

test('ignores a late breadcrumb from the previously selected target', async () => {
  getChildren.mockResolvedValueOnce({ items: [{ id: 'first', type: 'folder', name: '先选目录' }, { id: 'second', type: 'folder', name: '后选目录' }] });
  let resolveFirst;
  getBreadcrumb.mockImplementationOnce(() => new Promise(resolve => { resolveFirst = resolve; }))
    .mockResolvedValueOnce({ items: [{ name: '/' }, { name: '后选目录' }] });
  renderPicker();
  fireEvent.click(await screen.findByText('先选目录'));
  fireEvent.click(screen.getByText('后选目录'));
  expect(await screen.findByText('根目录 / 后选目录')).toBeInTheDocument();
  expect(getBreadcrumb.mock.calls[0][1].signal.aborted).toBe(true);
  await act(async () => resolveFirst({ items: [{ name: '/' }, { name: '先选目录' }] }));
  expect(screen.queryByText('根目录 / 先选目录')).not.toBeInTheDocument();
});
