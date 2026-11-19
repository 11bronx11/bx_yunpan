import React from 'react';
import { App as AntApp, ConfigProvider } from 'antd';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import Drive from './Drive';
import { createFolder, deleteFolder, getBreadcrumb, getChildren, getDownloadURL, getPreview, getRoot, renameFile } from '../services/drive';
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

test.each(['children', 'breadcrumb'])('shows a top message when opening a deleted folder fails in %s', async failure => {
  renderDrive();
  const row = await screen.findByRole('row', { name: '进入目录 空目录' });
  const error = Object.assign(new Error('文件或目录不存在，可能已被删除'), { status: 404, code: 'drive.not_found' });
  (failure === 'children' ? getChildren : getBreadcrumb).mockRejectedValueOnce(error);

  fireEvent.click(row);

  expect(await screen.findByText(error.message)).toBeInTheDocument();
  expect(within(screen.getByRole('main')).queryByText(error.message)).not.toBeInTheDocument();
  expect(within(screen.getByRole('main')).queryByRole('alert')).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: /根目录/ })).toHaveAttribute('aria-current', 'page');
  expect(row).toBeInTheDocument();
  expect(mockLogout).not.toHaveBeenCalled();
});

test.each([
  ['root', getRoot, '请求处理超时，请稍后重试'],
  ['children', getChildren, '请求处理超时，请稍后重试'],
  ['breadcrumb', getBreadcrumb, '请求处理超时，请稍后重试'],
  ['network', getRoot, '网络连接失败，请检查网络后重试'],
])('shows an initial %s failure at the top and can reload without refreshing the page', async (source, request, text) => {
  request.mockRejectedValueOnce(new Error(text));
  renderDrive();

  expect(await screen.findByText(text, { selector: '.ant-message-error span' })).toBeVisible();
  expect(within(screen.getByRole('main')).queryByRole('alert')).not.toBeInTheDocument();
  expect(screen.queryByText(text, { selector: '.ant-alert-message' })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /重新加载/ }));
  expect(await screen.findByRole('row', { name: '进入目录 空目录' })).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: /重新加载/ })).not.toBeInTheDocument();
  expect(mockLogout).not.toHaveBeenCalled();
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
  expect(screen.queryByText('问此文件')).not.toBeInTheDocument();
  fireEvent.click(await screen.findByText('下载'));

  await waitFor(() => expect(startDownload).toHaveBeenCalledWith('https://example.test/report.pdf', 'report.pdf'));
});

test.each([
  { kind: 'unsupported' },
  { kind: 'processing' },
  { kind: 'image', url: '' },
  { kind: 'unsupported', url: 'https://example.test/original.jpg' },
])('does not render an unavailable image preview: %j', async preview => {
  getChildren.mockResolvedValue({
    items: [{ id: 'huge-image', type: 'file', name: '三王山炼定仙游.jpg', mime_type: 'image/jpeg', size_bytes: 123076147, version: 1 }],
  });
  getPreview.mockResolvedValue(preview);
  renderDrive();

  const row = await screen.findByRole('row', { name: '预览图片 三王山炼定仙游.jpg' });
  await waitFor(() => expect(getPreview).toHaveBeenCalledTimes(1));
  expect(screen.queryByAltText('')).not.toBeInTheDocument();

  fireEvent.click(row);
  expect(await screen.findByText('图片暂时无法预览，可稍后重试或下载查看')).toBeInTheDocument();
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  expect(screen.queryByAltText('')).not.toBeInTheDocument();
});

test('renders and opens an available image thumbnail', async () => {
  getChildren.mockResolvedValue({
    items: [{ id: 'image', type: 'file', name: 'landscape.jpg', mime_type: 'image/jpeg', size_bytes: 42, version: 1 }],
  });
  getPreview.mockResolvedValue({ kind: 'image', url: 'https://example.test/thumbnail.jpg' });
  renderDrive();

  const row = await screen.findByRole('row', { name: '预览图片 landscape.jpg' });
  expect(await screen.findByAltText('')).toHaveAttribute('src', 'https://example.test/thumbnail.jpg');
  fireEvent.click(row);
  expect(await screen.findByRole('img', { name: 'landscape.jpg' })).toHaveAttribute('src', 'https://example.test/thumbnail.jpg');
  expect(getPreview).toHaveBeenCalledTimes(1);
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

test('shows a pagination failure at the top, keeps existing files, and can retry', async () => {
  getChildren.mockResolvedValueOnce({ items: [pageFile('first.pdf')], page: { has_more: true, next_cursor: 'page-2' } })
    .mockRejectedValueOnce(new Error('请求处理超时，请稍后重试'))
    .mockResolvedValueOnce({ items: [pageFile('second.pdf')], page: { has_more: false } });
  renderDrive();
  fireEvent.click(await screen.findByRole('button', { name: '加载更多' }));
  expect(await screen.findByText('请求处理超时，请稍后重试', { selector: '.ant-message-error span' })).toBeVisible();
  expect(screen.getByText('first.pdf')).toBeInTheDocument();
  expect(within(screen.getByRole('main')).queryByRole('alert')).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '加载更多' }));
  expect(await screen.findByText('second.pdf')).toBeInTheDocument();
  expect(screen.getByText('first.pdf')).toBeInTheDocument();
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

test('Enter on a folder action does not enter the folder', async () => {
  renderDrive();
  const button = await screen.findByRole('button', { name: '管理目录 空目录' });
  fireEvent.keyDown(button, { key: 'Enter' });
  expect(getChildren).toHaveBeenCalledTimes(1);
});

test('Enter on a file action does not open its summary', async () => {
  getChildren.mockResolvedValue({ items: [pageFile('report.pdf')] });
  getFileAI.mockResolvedValue({ status: 'indexed', summary: '摘要' });
  renderDrive();
  const button = await screen.findByRole('button', { name: '管理 report.pdf' });
  fireEvent.keyDown(button, { key: 'Enter' });
  expect(getFileAI).not.toHaveBeenCalled();
});

test('an opaque file row can receive keyboard focus to reveal desktop actions', async () => {
  getChildren.mockResolvedValue({ items: [{ ...pageFile('archive.bin'), mime_type: 'application/octet-stream' }] });
  renderDrive();
  const row = await screen.findByRole('row', { name: /archive.bin/ });
  expect(row).toHaveAttribute('tabindex', '0');
  fireEvent.click(row);
  fireEvent.keyDown(row, { key: 'Enter' });
  fireEvent.keyDown(row, { key: ' ' });
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  expect(getFileAI).not.toHaveBeenCalled();
  expect(getPreview).not.toHaveBeenCalled();
  expect(getDownloadURL).not.toHaveBeenCalled();
});

test('repeated Enter submits a folder creation only once while it is pending', async () => {
  createFolder.mockImplementationOnce(() => new Promise(() => {}));
  renderDrive();
  fireEvent.click(await screen.findByRole('button', { name: /新建目录/ }));
  const input = await screen.findByRole('textbox', { name: '目录名称' });
  fireEvent.change(input, { target: { value: '新目录' } });
  fireEvent.keyDown(input, { key: 'Enter' });
  fireEvent.keyUp(input, { key: 'Enter' });
  fireEvent.keyDown(input, { key: 'Enter' });
  expect(createFolder).toHaveBeenCalledTimes(1);
});

test('repeated Enter submits a rename only once while it is pending', async () => {
  getChildren.mockResolvedValue({ items: [pageFile('report.pdf')] });
  renameFile.mockImplementationOnce(() => new Promise(() => {}));
  renderDrive();
  fireEvent.click(await screen.findByRole('button', { name: '管理 report.pdf' }));
  fireEvent.click(await screen.findByText('重命名'));
  const input = await screen.findByRole('textbox', { name: '新文件名' });
  fireEvent.change(input, { target: { value: 'renamed.pdf' } });
  fireEvent.keyDown(input, { key: 'Enter' });
  fireEvent.keyUp(input, { key: 'Enter' });
  fireEvent.keyDown(input, { key: 'Enter' });
  expect(renameFile).toHaveBeenCalledTimes(1);
});
