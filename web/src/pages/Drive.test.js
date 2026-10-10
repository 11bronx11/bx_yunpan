import React from 'react';
import { App as AntApp, ConfigProvider } from 'antd';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import Drive from './Drive';
import { deleteFolder, getBreadcrumb, getChildren, getDownloadURL, getRoot } from '../services/drive';
import { getFileAI } from '../services/ai';
import { startDownload } from '../utils/download';

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
jest.mock('../utils/download', () => ({ startDownload: jest.fn() }));

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
  renderDrive();

  fireEvent.click(await screen.findByRole('button', { name: '管理目录 空目录' }));
  fireEvent.click(await screen.findByText('删除目录'));

  const dialog = within(await screen.findByRole('dialog'));
  expect(dialog.getByText('只有完全空的目录才能删除。')).toBeInTheDocument();
  fireEvent.click(dialog.getByRole('button', { name: /删\s*除/ }));

  await waitFor(() => expect(deleteFolder).toHaveBeenCalledWith('folder-1', 2));
  await waitFor(() => expect(getChildren).toHaveBeenCalledTimes(2));
});

test('opens the stored AI summary for an octet-stream source file', async () => {
  getChildren.mockResolvedValue({
    items: [{ id: 'file-1', type: 'file', name: 'main.cpp', mime_type: 'application/octet-stream', size_bytes: 42, version: 1, ai_status: 'indexed' }],
  });
  getFileAI.mockResolvedValue({ status: 'indexed', summary: '一个包含程序入口的 C++ 示例。', tags: ['C++'], language: 'zh' });

  renderDrive();

  fireEvent.click(await screen.findByText('main.cpp'));

  await waitFor(() => expect(getFileAI).toHaveBeenCalledWith('file-1', expect.objectContaining({ signal: expect.any(AbortSignal) })));
  expect(await screen.findByText('一个包含程序入口的 C++ 示例。')).toBeInTheDocument();
});

test('downloads a file without opening a new window', async () => {
  getChildren.mockResolvedValue({
    items: [{ id: 'file-1', type: 'file', name: 'report.pdf', mime_type: 'application/pdf', size_bytes: 42, version: 1 }],
  });
  getDownloadURL.mockResolvedValue({ url: 'https://example.test/report.pdf' });

  renderDrive();

  fireEvent.click(await screen.findByRole('button', { name: '管理 report.pdf' }));
  fireEvent.click(await screen.findByText('下载'));

  await waitFor(() => expect(startDownload).toHaveBeenCalledWith('https://example.test/report.pdf', 'report.pdf'));
});

// Exercise the real menu and confirmation without waiting for CSS transitions
// in jsdom; scope confirmation queries to avoid scanning the whole page.
const renderDrive = () => render(<ConfigProvider theme={{ token: { motion: false } }}><AntApp><MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}><Drive /></MemoryRouter></AntApp></ConfigProvider>);
const pageFile = name => ({ id: name, type: 'file', name, mime_type: 'application/pdf', size_bytes: 42, version: 1 });

test('loads one bounded page and appends the next page on demand', async () => {
  getChildren.mockResolvedValueOnce({ items: [pageFile('first.pdf')], page: { has_more: true, next_cursor: 'page-2' } })
    .mockResolvedValueOnce({ items: [pageFile('second.pdf')], page: { has_more: false, next_cursor: '' } });
  const view = renderDrive();
  expect(await screen.findByText('first.pdf')).toBeInTheDocument();
  expect(getChildren).toHaveBeenCalledTimes(1);
  expect(getChildren).toHaveBeenNthCalledWith(1, 'root', expect.objectContaining({ limit: 50, signal: expect.any(AbortSignal) }));
  fireEvent.click(screen.getByRole('button', { name: '加载更多' }));
  expect(await screen.findByText('second.pdf')).toBeInTheDocument();
  expect(screen.getByText('first.pdf')).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: '加载更多' })).not.toBeInTheDocument();
  expect(getChildren).toHaveBeenNthCalledWith(2, 'root', expect.objectContaining({ cursor: 'page-2', limit: 50 }));
  expect(getBreadcrumb).toHaveBeenCalledTimes(1);
  const signal = getChildren.mock.calls[1][1].signal;
  view.unmount();
  expect(signal.aborted).toBe(true);
});

test('aborts a previous directory request and ignores its late result', async () => {
  let resolveOld;
  getChildren.mockResolvedValueOnce({ items: [{ id: 'folder-1', type: 'folder', name: '子目录', version: 1 }] })
    .mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve; }))
    .mockResolvedValueOnce({ items: [pageFile('current.pdf')] });
  renderDrive();
  fireEvent.click(await screen.findByText('子目录'));
  await waitFor(() => expect(getChildren).toHaveBeenCalledTimes(2));
  const oldSignal = getChildren.mock.calls[1][1].signal;
  fireEvent.click(screen.getByRole('button', { name: /根目录/ }));
  expect(await screen.findByText('current.pdf')).toBeInTheDocument();
  expect(oldSignal.aborted).toBe(true);
  await act(async () => resolveOld({ items: [pageFile('obsolete.pdf')] }));
  expect(screen.queryByText('obsolete.pdf')).not.toBeInTheDocument();
  expect(screen.getByText('current.pdf')).toBeInTheDocument();
});

test('restarts from page one when a cursor expires without appending stale rows', async () => {
  getChildren.mockResolvedValueOnce({ items: [pageFile('old.pdf')], page: { has_more: true, next_cursor: 'old-cursor' } })
    .mockRejectedValueOnce(Object.assign(new Error('changed'), { status: 409, code: 'drive.cursor_stale' }))
    .mockResolvedValueOnce({ items: [pageFile('fresh.pdf')], page: { has_more: false } });
  renderDrive();
  fireEvent.click(await screen.findByRole('button', { name: '加载更多' }));
  expect(await screen.findByText('fresh.pdf')).toBeInTheDocument();
  expect(screen.queryByText('old.pdf')).not.toBeInTheDocument();
  expect(getChildren).toHaveBeenNthCalledWith(3, 'root', expect.objectContaining({ cursor: '' }));
});
