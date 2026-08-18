import React from 'react';
import { App } from 'antd';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import SearchResultModal from './SearchResultModal';
import {
  deleteFile,
  getBreadcrumb,
  getChildren,
  getDownloadURL,
  getRoot,
  moveFile,
  renameFile,
} from '../services/drive';
import { createShare } from '../services/share';

jest.mock('../services/drive', () => ({
  deleteFile: jest.fn(),
  getBreadcrumb: jest.fn(),
  getChildren: jest.fn(),
  getDownloadURL: jest.fn(),
  getRoot: jest.fn(),
  moveFile: jest.fn(),
  renameFile: jest.fn(),
}));
jest.mock('../services/share', () => ({ createShare: jest.fn() }));

const hit = {
  file: {
    id: 'file-1',
    folder_id: 'folder-1',
    name: 'report.pdf',
    mime_type: 'application/pdf',
    size_bytes: 2048,
    version: 3,
  },
  match_type: 'hybrid',
  citations: [{ id: 'citation-1', excerpt: '项目风险与后续计划', page_number: 2 }],
};

beforeEach(() => {
  jest.clearAllMocks();
  getBreadcrumb.mockResolvedValue({ items: [{ id: 'root', name: '/' }, { id: 'folder-1', name: '资料' }] });
  getChildren.mockResolvedValue({ items: [] });
  getRoot.mockResolvedValue({ id: 'root', name: '/' });
  getDownloadURL.mockResolvedValue({ url: 'https://example.test/report.pdf' });
  createShare.mockResolvedValue({ share_key: 'share-key' });
  renameFile.mockResolvedValue({ ...hit.file, name: 'renamed.pdf', version: 4 });
  deleteFile.mockResolvedValue(null);
  moveFile.mockResolvedValue({ ...hit.file, folder_id: 'root', version: 4 });
});

const renderModal = (props = {}) => {
  const onClose = jest.fn();
  const onChanged = jest.fn().mockResolvedValue(undefined);
  render(<App><SearchResultModal hit={hit} open onClose={onClose} onChanged={onChanged} {...props} /></App>);
  return { onClose, onChanged };
};

test('shows file context and all drive actions', async () => {
  renderModal();

  expect(await screen.findByText('根目录 / 资料')).toBeInTheDocument();
  expect(screen.getByText('“项目风险与后续计划”')).toBeInTheDocument();
  for (const name of ['下载', '分享', '移动到', '重命名', '删除']) {
    expect(screen.getByRole('button', { name })).toBeInTheDocument();
  }
});

test('renames a search result and refreshes the search', async () => {
  const { onClose, onChanged } = renderModal();

  await screen.findByText('根目录 / 资料');
  fireEvent.click(screen.getByRole('button', { name: '重命名' }));
  fireEvent.change(screen.getByLabelText('新文件名'), { target: { value: 'renamed.pdf' } });
  fireEvent.click(screen.getByRole('button', { name: /保\s*存/ }));

  await waitFor(() => expect(renameFile).toHaveBeenCalledWith('file-1', 3, 'renamed.pdf'));
  expect(onChanged).toHaveBeenCalledTimes(1);
  expect(onClose).toHaveBeenCalledTimes(1);
});

test('opens the existing directory picker for moving', async () => {
  renderModal();

  await screen.findByText('根目录 / 资料');
  fireEvent.click(screen.getByRole('button', { name: '移动到' }));

  expect(await screen.findByRole('tree', { name: '目标目录' })).toBeInTheDocument();
  expect(getRoot).toHaveBeenCalledTimes(1);
});
