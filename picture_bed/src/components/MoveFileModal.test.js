import React from 'react';
import { App as AntApp } from 'antd';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
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
