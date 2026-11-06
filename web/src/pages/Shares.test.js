import React from 'react';
import { App, ConfigProvider } from 'antd';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import Shares, { shareStatus } from './Shares';
import { listShares, revokeShare } from '../services/share';

jest.mock('../services/share', () => ({ listShares: jest.fn(), revokeShare: jest.fn(), updateShare: jest.fn() }));
beforeEach(() => jest.clearAllMocks());
const share = (id, extra = {}) => ({ id, file_id: 'file-1', file_name: `${id}.pdf`, file_available: true, version: 1, created_at: new Date().toISOString(), ...extra });
const show = () => render(<ConfigProvider theme={{ token: { motion: false } }}><App><Shares /></App></ConfigProvider>);

test('derives expiration without waiting for a server refresh', () => {
  expect(shareStatus(share('one', { expires_at: '2026-01-01' }), Date.parse('2026-02-01'))).toBe('expired');
  expect(shareStatus(share('one', { revoked_at: '2026-01-01', file_available: false }))).toBe('revoked');
  expect(shareStatus(share('one', { file_available: false }))).toBe('unavailable');
});

test('shows states, appends pages and cancels a share after confirmation', async () => {
  const one = share('one');
  listShares.mockResolvedValueOnce({ items: [one], page: { has_more: true, next_cursor: 'page-2' } })
    .mockResolvedValueOnce({ items: [share('two', { file_available: false })], page: {} })
    .mockResolvedValueOnce({ items: [share('one', { revoked_at: '2026-01-01' })], page: {} });
  revokeShare.mockResolvedValue(null);
  const view = show();
  expect(await screen.findByText('one.pdf')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '加载更多' }));
  expect(await screen.findByText('two.pdf')).toBeInTheDocument();
  expect(listShares.mock.calls[1][0].cursor).toBe('page-2');
  expect(screen.getByText('源文件失效')).toBeInTheDocument();
  const row = screen.getByRole('row', { name: /one.pdf/ });
  fireEvent.click(within(row).getByRole('button', { name: '取消分享' }));
  const dialog = within(await screen.findByRole('dialog'));
  expect(revokeShare).not.toHaveBeenCalled();
  fireEvent.click(dialog.getByRole('button', { name: '取消分享' }));
  expect(await screen.findByText('已撤销')).toBeInTheDocument();
  expect(revokeShare).toHaveBeenCalledWith('one');
  const signal = listShares.mock.calls.at(-1)[0].signal;
  view.unmount(); expect(signal.aborted).toBe(true);
});

test('shows a recoverable list error', async () => {
  listShares.mockRejectedValueOnce(new Error('网络连接失败')).mockResolvedValueOnce({ items: [], page: {} });
  show();
  expect(await screen.findByText('网络连接失败')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /重\s*试/ }));
  await waitFor(() => expect(listShares).toHaveBeenCalledTimes(2));
  expect(await screen.findByText('还没有分享')).toBeInTheDocument();
});
