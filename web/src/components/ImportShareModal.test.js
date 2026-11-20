import React from 'react';
import { App, ConfigProvider } from 'antd';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import ImportShareModal from './ImportShareModal';
import { importShare, resolveShare } from '../services/share';
import { clientError } from '../services/api';

jest.mock('../services/share', () => ({ ...jest.requireActual('../services/share'), importShare: jest.fn(), resolveShare: jest.fn() }));
const resolved = { share: { id: 'share-1', file_name: 'report.pdf', expires_at: null }, share_access_token: 'token' };
beforeEach(() => { jest.clearAllMocks(); Object.defineProperty(global, 'crypto', { configurable: true, value: { randomUUID: jest.fn(() => 'retry-key') } }); });
const show = props => render(<ConfigProvider theme={{ token: { motion: false } }}><App><ImportShareModal open folderId="root" targetLabel="根目录" onCancel={jest.fn()} {...props} /></App></ConfigProvider>);
const enterKey = () => fireEvent.change(screen.getByLabelText('分享 Key'), { target: { value: 'a'.repeat(22) } });

test.each([
  ['   ', '请输入分享 Key', 'warning'],
  ['wrong-key', '分享key格式错误', 'error'],
  ['a'.repeat(23), '分享key格式错误', 'error'],
  ['a'.repeat(21) + '!', '分享key格式错误', 'error'],
])('explains invalid key input %s without resolving it', async (key, message, type) => {
  show();
  fireEvent.change(screen.getByLabelText('分享 Key'), { target: { value: key } });
  fireEvent.click(screen.getByRole('button', { name: '解析分享' }));
  expect(await screen.findByText(message)).toBeInTheDocument();
  expect(screen.getByRole('img', { name: type === 'error' ? 'close-circle' : 'exclamation-circle' })).toBeInTheDocument();
  expect(resolveShare).not.toHaveBeenCalled();
});

test.each([
  ['share.not_found', '分享不存在或 Key 不正确，请检查后重试'],
  ['share.source_deleted', '分享的源文件已被删除，无法导入'],
  ['share.expired', '分享已过期，请联系分享者重新分享'],
  ['share.revoked', '分享已被撤销，请联系分享者重新分享'],
])('explains resolution failure %s without asking for a complete key', async (code, message) => {
  resolveShare.mockRejectedValueOnce(clientError(code));
  show(); enterKey();
  fireEvent.click(screen.getByRole('button', { name: '解析分享' }));
  expect(await screen.findByText(message)).toBeInTheDocument();
  expect(screen.queryByText('请输入完整的分享 Key')).not.toBeInTheDocument();
  expect(importShare).not.toHaveBeenCalled();
});

test.each(['share.source_deleted', 'share.expired', 'share.revoked', 'share.access_expired'])('clears the preview when %s happens after resolution', async code => {
  resolveShare.mockResolvedValueOnce(resolved);
  importShare.mockRejectedValueOnce(clientError(code));
  const onImported = jest.fn(); const onCancel = jest.fn();
  show({ onImported, onCancel }); enterKey();
  fireEvent.click(screen.getByRole('button', { name: '解析分享' }));
  fireEvent.click(await screen.findByRole('button', { name: '导入到当前目录' }));
  expect(await screen.findByText(clientError(code).message)).toBeInTheDocument();
  expect(screen.queryByText('report.pdf')).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: '解析分享' })).toBeInTheDocument();
  expect(onImported).not.toHaveBeenCalled();
  expect(onCancel).not.toHaveBeenCalled();
});

test('ignores a stale resolution after the user changes the key', async () => {
  let complete;
  resolveShare.mockImplementation(() => new Promise(resolve => { complete = resolve; }));
  show(); enterKey();
  fireEvent.click(screen.getByRole('button', { name: '解析分享' }));
  await waitFor(() => expect(resolveShare).toHaveBeenCalledTimes(1));
  fireEvent.change(screen.getByLabelText('分享 Key'), { target: { value: 'b'.repeat(22) } });
  expect(resolveShare.mock.calls[0][1].signal.aborted).toBe(true);
  await act(async () => complete(resolved));
  expect(screen.queryByText('report.pdf')).not.toBeInTheDocument();
  expect(screen.queryByRole('button', { name: '导入到当前目录' })).not.toBeInTheDocument();
});

test('reuses the same idempotency key after a failed import request', async () => {
  resolveShare.mockResolvedValue(resolved);
  importShare.mockRejectedValueOnce(new Error('网络连接失败')).mockResolvedValueOnce({});
  const onImported = jest.fn(); show({ onImported }); enterKey();
  fireEvent.click(screen.getByRole('button', { name: '解析分享' }));
  fireEvent.click(await screen.findByRole('button', { name: '导入到当前目录' }));
  await screen.findByText('网络连接失败');
  fireEvent.click(screen.getByRole('button', { name: '导入到当前目录' }));
  await waitFor(() => expect(onImported).toHaveBeenCalledTimes(1));
  expect(importShare.mock.calls).toEqual([
    ['share-1', 'root', 'token', 'retry-key'], ['share-1', 'root', 'token', 'retry-key'],
  ]);
});

test('imports with a stable retry key when randomUUID is unavailable', async () => {
  crypto.randomUUID = undefined;
  resolveShare.mockResolvedValue(resolved);
  importShare.mockRejectedValueOnce(new Error('网络连接失败')).mockResolvedValueOnce({});
  const onImported = jest.fn(); show({ onImported }); enterKey();
  fireEvent.click(screen.getByRole('button', { name: '解析分享' }));
  fireEvent.click(await screen.findByRole('button', { name: '导入到当前目录' }));
  await screen.findByText('网络连接失败');
  fireEvent.click(screen.getByRole('button', { name: '导入到当前目录' }));
  await waitFor(() => expect(onImported).toHaveBeenCalledTimes(1));
  const key = importShare.mock.calls[0][3];
  expect(key).toEqual(expect.any(String));
  expect(key.length).toBeGreaterThan(0);
  expect(importShare.mock.calls[1][3]).toBe(key);
});
