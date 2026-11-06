import React from 'react';
import { App, ConfigProvider } from 'antd';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import ImportShareModal from './ImportShareModal';
import { importShare, resolveShare } from '../services/share';

jest.mock('../services/share', () => ({ importShare: jest.fn(), resolveShare: jest.fn() }));
const resolved = { share: { id: 'share-1', file_name: 'report.pdf', expires_at: null }, share_access_token: 'token' };
beforeEach(() => { jest.clearAllMocks(); Object.defineProperty(global, 'crypto', { configurable: true, value: { randomUUID: jest.fn(() => 'retry-key') } }); });
const show = props => render(<ConfigProvider theme={{ token: { motion: false } }}><App><ImportShareModal open folderId="root" targetLabel="根目录" onCancel={jest.fn()} {...props} /></App></ConfigProvider>);
const enterKey = () => fireEvent.change(screen.getByLabelText('分享 Key'), { target: { value: 'a'.repeat(22) } });

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
