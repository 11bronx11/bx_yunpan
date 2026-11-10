import React from 'react';
import { App, ConfigProvider } from 'antd';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import Shares, { shareStatus } from './Shares';
import { listShares, revokeShare } from '../services/share';

jest.mock('../services/share', () => ({ listShares: jest.fn(), revokeShare: jest.fn(), updateShare: jest.fn() }));
jest.setTimeout(15000);
beforeEach(() => jest.clearAllMocks());
const share = (id, extra = {}) => ({ id, file_id: 'file-1', file_name: `${id}.pdf`, file_available: true, status: 'active', version: 1, created_at: new Date().toISOString(), ...extra });
const show = () => render(<ConfigProvider theme={{ token: { motion: false } }}><App><Shares /></App></ConfigProvider>);

test('derives expiration and respects server read-only states', () => {
  expect(shareStatus(share('one', { expires_at: '2026-01-01' }), Date.parse('2026-02-01'))).toBe('expired');
  expect(shareStatus(share('one', { status: 'expired' }))).toBe('expired');
  expect(shareStatus(share('one', { revoked_at: '2026-01-01', file_available: false }))).toBe('revoked');
  expect(shareStatus(share('one', { source_deleted_at: '2026-01-01' }))).toBe('source_deleted');
});

test('switches filtered tabs, replaces pages, and navigates back with the correct cursor', async () => {
  listShares.mockResolvedValueOnce({ items: [share('one')], page: { has_more: true, next_cursor: 'page-2' } })
    .mockResolvedValueOnce({ items: [share('two')], page: {} })
    .mockResolvedValueOnce({ items: [share('one')], page: { has_more: true, next_cursor: 'page-2' } })
    .mockResolvedValueOnce({ items: [share('expired', { status: 'expired' })], page: {} })
    .mockResolvedValueOnce({ items: [share('revoked', { status: 'revoked' })], page: {} });
  const view = show();
  expect(await screen.findByText('one.pdf')).toBeInTheDocument();
  expect(listShares).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'active', limit: 10, cursor: '' }));
  expect(screen.getByRole('button', { name: '上一页' })).toBeDisabled();
  fireEvent.click(screen.getByRole('button', { name: '下一页' }));
  expect(await screen.findByText('two.pdf')).toBeInTheDocument();
  expect(screen.queryByText('one.pdf')).not.toBeInTheDocument();
  expect(listShares).toHaveBeenLastCalledWith(expect.objectContaining({ cursor: 'page-2' }));
  expect(screen.getByText('第 2 页 · 每页 10 条')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '上一页' }));
  expect(await screen.findByText('one.pdf')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('tab', { name: '分享过期' }));
  expect(await screen.findByText('expired.pdf')).toBeInTheDocument();
  expect(listShares).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'expired', cursor: '' }));
  expect(screen.queryByRole('button', { name: '修改有效期' })).not.toBeInTheDocument();
  expect(screen.queryByRole('button', { name: '提前撤销' })).not.toBeInTheDocument();
  const row = screen.getByRole('row', { name: /expired.pdf/ });
  fireEvent.click(within(row).getByRole('button', { name: /查\s*看/ }));
  expect(await screen.findByText('分享详情')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /关\s*闭/ }));
  fireEvent.click(screen.getByRole('tab', { name: '分享撤销' }));
  expect(await screen.findByText('revoked.pdf')).toBeInTheDocument();
  expect(listShares).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'revoked', cursor: '' }));
  expect(screen.queryByRole('button', { name: '修改有效期' })).not.toBeInTheDocument();
  const signal = listShares.mock.calls.at(-1)[0].signal;
  view.unmount(); expect(signal.aborted).toBe(true);
});

test('requires confirmation before revoking an active share and refreshes the list', async () => {
  listShares.mockResolvedValueOnce({ items: [share('one')], page: {} }).mockResolvedValue({ items: [], page: {} });
  revokeShare.mockResolvedValue(null);
  show();
  const row = await screen.findByRole('row', { name: /one.pdf/ });
  fireEvent.click(within(row).getByRole('button', { name: '提前撤销' }));
  const dialog = within(await screen.findByRole('dialog'));
  expect(revokeShare).not.toHaveBeenCalled();
  fireEvent.click(dialog.getByRole('button', { name: '撤销分享' }));
  await waitFor(() => expect(revokeShare).toHaveBeenCalledWith('one'));
  expect(await screen.findByText('暂无有效分享')).toBeInTheDocument();
});

test('ignores a late response from a previously selected tab', async () => {
  let finish;
  listShares.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
    .mockResolvedValueOnce({ items: [share('expired', { status: 'expired' })], page: {} });
  show();
  await waitFor(() => expect(listShares).toHaveBeenCalledTimes(1));
  const previous = listShares.mock.calls[0][0].signal;
  fireEvent.click(screen.getByRole('tab', { name: '分享过期' }));
  expect(await screen.findByText('expired.pdf')).toBeInTheDocument();
  expect(previous.aborted).toBe(true);
  finish({ items: [share('stale')], page: {} });
  await waitFor(() => expect(screen.queryByText('stale.pdf')).not.toBeInTheDocument());
});

test('shows a recoverable list error without informational blue alerts', async () => {
  listShares.mockRejectedValueOnce(new Error('网络连接失败')).mockResolvedValueOnce({ items: [], page: {} });
  show();
  expect(await screen.findByText('网络连接失败')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /重\s*试/ }));
  expect(await screen.findByText('暂无有效分享')).toBeInTheDocument();
  expect(screen.queryByRole('alert')).not.toBeInTheDocument();
});


test('deleted source history has its own tab and only offers viewing', async () => {
  listShares.mockResolvedValueOnce({ items: [], page: {} }).mockResolvedValueOnce({
    items: [share('history', { status: 'source_deleted', source_deleted_at: '2026-01-01', revoked_at: '2026-01-01', file_available: false, mime_type: 'application/pdf' })], page: {},
  });
  show();
  await screen.findByText('暂无有效分享');
  fireEvent.click(screen.getByRole('tab', { name: '源文件已删除' }));
  const row = await screen.findByRole('row', { name: /history.pdf/ });
  expect(listShares).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'source_deleted', limit: 10 }));
  expect(within(row).getByText('PDF')).toBeInTheDocument();
  expect(within(row).getAllByRole('button')).toHaveLength(1);
  expect(within(row).getByRole('button', { name: /查\s*看/ })).toBeInTheDocument();
  expect(shareStatus(share('both', { revoked_at: '2026-01-01', source_deleted_at: '2026-01-01' }))).toBe('source_deleted');
});
