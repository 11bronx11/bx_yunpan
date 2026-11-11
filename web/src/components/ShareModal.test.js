import React from 'react';
import { App, ConfigProvider } from 'antd';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import ShareModal from './ShareModal';
import { createShare, updateShare } from '../services/share';

jest.mock('../services/share', () => ({ createShare: jest.fn(), updateShare: jest.fn() }));
beforeEach(() => { jest.clearAllMocks(); createShare.mockResolvedValue({ share_key: 'saved-key', share: {} }); updateShare.mockResolvedValue({}); });
const show = props => render(<ConfigProvider theme={{ token: { motion: false } }}><App><ShareModal open file={{ id: 'file-1', name: 'report.pdf' }} onCancel={jest.fn()} {...props} /></App></ConfigProvider>);

test('creates only after confirmation and defaults to seven days', async () => {
  show();
  expect(createShare).not.toHaveBeenCalled();
  const before = Date.now();
  fireEvent.click(screen.getByRole('button', { name: '创建分享' }));
  expect(await screen.findByLabelText('分享 Key')).toHaveValue('saved-key');
  const [file, expiry] = createShare.mock.calls[0];
  expect(file).toBe('file-1');
  expect(new Date(expiry).valueOf() - before).toBeGreaterThan(7 * 86400000 - 1000);
  expect(screen.getByText(/关闭后无法再次查看/)).toBeInTheDocument();
});

test('allows a permanent share and prevents duplicate submission', async () => {
  createShare.mockImplementation(() => new Promise(() => {}));
  show();
  fireEvent.mouseDown(screen.getByRole('combobox', { name: '有效期' }));
  fireEvent.click(await screen.findByText('长期有效'));
  const button = screen.getByRole('button', { name: '创建分享' });
  fireEvent.click(button); fireEvent.click(button);
  expect(createShare).toHaveBeenCalledTimes(1);
  expect(createShare).toHaveBeenCalledWith('file-1', null);
});

test('rejects a custom expiry until a future date is selected', async () => {
  show();
  fireEvent.mouseDown(screen.getByRole('combobox', { name: '有效期' }));
  fireEvent.click(await screen.findByText('自定义时间'));
  fireEvent.click(screen.getByRole('button', { name: '创建分享' }));
  expect(await screen.findByText('请选择未来的过期时间')).toBeInTheDocument();
  expect(createShare).not.toHaveBeenCalled();
});

test('refreshes and closes an edit when another device changed its version', async () => {
  updateShare.mockRejectedValue(Object.assign(new Error('分享已被其他操作修改'), { code: 'share.version_conflict' }));
  const onSaved = jest.fn(), onCancel = jest.fn();
  show({ share: { id: 'share-1', version: 3 }, onSaved, onCancel });
  fireEvent.click(screen.getByRole('button', { name: /保\s*存/ }));
  await waitFor(() => expect(updateShare).toHaveBeenCalledWith('share-1', 3, expect.any(String)));
  await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
  expect(onCancel).toHaveBeenCalledTimes(1);
});


test('recalculates edited duration from save time rather than the old deadline', async () => {
  const savedAt = Date.now();
  const clock = jest.spyOn(Date, 'now').mockReturnValue(savedAt);
  try {
    show({ share: { id: 'share-1', version: 3, expires_at: '2099-01-01T00:00:00Z' } });
    fireEvent.click(screen.getByRole('button', { name: /保\s*存/ }));
    await waitFor(() => expect(updateShare).toHaveBeenCalledTimes(1));
    const expires = new Date(updateShare.mock.calls[0][2]).valueOf();
    expect(expires).toBe(savedAt + 7 * 86400000);
  } finally { clock.mockRestore(); }
});

test.each(['expired', 'revoked', 'source_deleted'])('only displays inactive share %s', status => {
  show({ share: { id: 'share-1', status, version: 3 } });
  expect(screen.queryByRole('button', { name: /保\s*存/ })).not.toBeInTheDocument();
  expect(screen.queryByRole('combobox')).not.toBeInTheDocument();
  expect(screen.getByText('分享已失效，仅可查看。')).toBeInTheDocument();
  expect(screen.queryByRole('alert')).not.toBeInTheDocument();
});

test('rechecks expiry at click time even before the display timer fires', async () => {
  const before = Date.now();
  const clock = jest.spyOn(Date, 'now').mockReturnValue(before);
  const onSaved = jest.fn(), onCancel = jest.fn();
  try {
    show({ share: { id: 'share-1', version: 3, expires_at: new Date(before + 500).toISOString() }, onSaved, onCancel });
    clock.mockReturnValue(before + 501);
    fireEvent.click(screen.getByRole('button', { name: /保\s*存/ }));
    expect(updateShare).not.toHaveBeenCalled();
    expect(onSaved).toHaveBeenCalledTimes(1);
    expect(onCancel).toHaveBeenCalledTimes(1);
  } finally { clock.mockRestore(); }
});

test('refreshes an old dialog when the backend reports expiration', async () => {
  updateShare.mockRejectedValue(Object.assign(new Error('分享已失效'), { code: 'share.inactive' }));
  const onSaved = jest.fn(), onCancel = jest.fn();
  show({ share: { id: 'share-1', version: 3 }, onSaved, onCancel });
  fireEvent.click(screen.getByRole('button', { name: /保\s*存/ }));
  await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
  expect(onCancel).toHaveBeenCalledTimes(1);
});
