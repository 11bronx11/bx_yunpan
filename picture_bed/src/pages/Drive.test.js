import React from 'react';
import { App as AntApp } from 'antd';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import Drive from './Drive';
import { deleteFolder, getBreadcrumb, getChildren, getRoot } from '../services/drive';
import { getFileAI } from '../services/ai';

const mockLogout = jest.fn();
const mockEnqueueUploads = jest.fn();

jest.mock('../contexts/AuthContext', () => ({ useAuth: () => ({ logout: mockLogout }) }));
jest.mock('../contexts/TransferContext', () => ({ useTransfers: () => ({ enqueueUploads: mockEnqueueUploads }) }));
jest.mock('../services/drive', () => ({
  createFolder: jest.fn(),
  deleteFile: jest.fn(),
  deleteFolder: jest.fn(),
  getBreadcrumb: jest.fn(),
  getChildren: jest.fn(),
  getDownloadURL: jest.fn(),
  getPreview: jest.fn(),
  getRoot: jest.fn(),
  renameFile: jest.fn(),
}));
jest.mock('../services/share', () => ({
  createShare: jest.fn(),
  importShare: jest.fn(),
  resolveShare: jest.fn(),
}));
jest.mock('../services/ai', () => ({
  getFileAI: jest.fn(),
  reprocessFileAI: jest.fn(),
}));

beforeEach(() => {
  jest.clearAllMocks();
  getRoot.mockResolvedValue({ id: 'root', name: '/', version: 1 });
  getBreadcrumb.mockResolvedValue({ items: [{ id: 'root', name: '/', version: 1 }] });
  getChildren.mockResolvedValue({
    items: [{ id: 'folder-1', type: 'folder', name: '空目录', version: 2 }],
  });
  deleteFolder.mockResolvedValue(null);
});

test('deletes a folder from the compact row menu after confirmation', async () => {
  render(<AntApp><MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}><Drive /></MemoryRouter></AntApp>);

  fireEvent.click(await screen.findByRole('button', { name: '管理目录 空目录' }));
  fireEvent.click(await screen.findByText('删除目录'));

  expect(await screen.findByText('只有完全空的目录才能删除。')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /删\s*除/ }));

  await waitFor(() => expect(deleteFolder).toHaveBeenCalledWith('folder-1', 2));
  await waitFor(() => expect(getChildren).toHaveBeenCalledTimes(2));
});

test('opens the stored AI summary for an octet-stream source file', async () => {
  getChildren.mockResolvedValue({
    items: [{ id: 'file-1', type: 'file', name: 'main.cpp', mime_type: 'application/octet-stream', size_bytes: 42, version: 1, ai_status: 'indexed' }],
  });
  getFileAI.mockResolvedValue({ status: 'indexed', summary: '一个包含程序入口的 C++ 示例。', tags: ['C++'], language: 'zh' });

  render(<AntApp><MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}><Drive /></MemoryRouter></AntApp>);

  fireEvent.click(await screen.findByText('main.cpp'));

  await waitFor(() => expect(getFileAI).toHaveBeenCalledWith('file-1', expect.objectContaining({ signal: expect.any(AbortSignal) })));
  expect(await screen.findByText('一个包含程序入口的 C++ 示例。')).toBeInTheDocument();
});
