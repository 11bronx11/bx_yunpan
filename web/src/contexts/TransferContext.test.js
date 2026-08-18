import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { abortUpload, listActiveUploads, uploadFile } from '../services/drive';
import { TransferProvider, useTransfers } from './TransferContext';

jest.mock('./AuthContext', () => ({
  useAuth: () => ({ user: { id: 'user-1' }, ready: true }),
}));

jest.mock('../services/drive', () => ({
  abortUpload: jest.fn(() => Promise.resolve()),
  listActiveUploads: jest.fn(),
  uploadFile: jest.fn(),
  waitForUpload: jest.fn(),
}));

const Probe = () => {
  const { uploads, enqueueUploads } = useTransfers();
  return <div>
    <button type="button" onClick={() => enqueueUploads([new File(['data'], 'sample.txt', { type: 'text/plain' })], 'folder-1', '根目录')}>添加</button>
    {uploads.map(task => <span key={task.id} data-error-code={task.errorCode}>{task.status}:{task.name}:{task.progress}</span>)}
  </div>;
};

const renderTransfers = () => render(<TransferProvider><Probe /></TransferProvider>);

beforeEach(() => {
  localStorage.clear();
  jest.clearAllMocks();
  abortUpload.mockResolvedValue();
});

test('restores an active multipart session as interrupted after refresh', async () => {
  localStorage.setItem('bx-yunpan:uploads:user-1', JSON.stringify([{
    id: 'task-1',
    folderId: 'folder-1',
    destination: '根目录',
    name: 'resume.bin',
    size: 100,
    session: { id: 'upload-1' },
    createdAt: 1,
  }]));
  listActiveUploads.mockResolvedValue({ items: [{
    id: 'upload-1',
    status: 'uploading',
    folder_id: 'folder-1',
    filename: 'resume.bin',
    sha256: 'a'.repeat(64),
    size_bytes: 100,
    confirmed_parts: [{ part_number: 1, size_bytes: 50 }],
    created_at: new Date().toISOString(),
  }] });

  renderTransfers();

  expect(await screen.findByText('interrupted:resume.bin:45')).toBeInTheDocument();
});

test('keeps completed uploads visible but removes them from refresh storage', async () => {
  listActiveUploads.mockResolvedValue({ items: [] });
  uploadFile.mockResolvedValue({ instant: true, file: { id: 'file-1' } });
  renderTransfers();
  await waitFor(() => expect(listActiveUploads).toHaveBeenCalled());

  fireEvent.click(screen.getByRole('button', { name: '添加' }));

  expect(await screen.findByText('completed:sample.txt:100')).toBeInTheDocument();
  await waitFor(() => expect(localStorage.getItem('bx-yunpan:uploads:user-1')).toBeNull());
});

test('treats an existing file as skipped instead of failed', async () => {
  listActiveUploads.mockResolvedValue({ items: [] });
  const error = new Error('网盘中已存在该文件，本次未重复上传');
  error.code = 'upload.file_exists';
  uploadFile.mockRejectedValue(error);
  renderTransfers();
  await waitFor(() => expect(listActiveUploads).toHaveBeenCalled());

  fireEvent.click(screen.getByRole('button', { name: '添加' }));

  expect(await screen.findByText('skipped:sample.txt:100')).toBeInTheDocument();
});

test('preserves the name-conflict reason for the transfer status', async () => {
  listActiveUploads.mockResolvedValue({ items: [] });
  const error = new Error('当前目录已存在同名文件，请重命名后上传');
  error.code = 'upload.name_conflict';
  uploadFile.mockRejectedValue(error);
  renderTransfers();
  await waitFor(() => expect(listActiveUploads).toHaveBeenCalled());

  fireEvent.click(screen.getByRole('button', { name: '添加' }));

  expect(await screen.findByText('skipped:sample.txt:0')).toHaveAttribute('data-error-code', 'upload.name_conflict');
});
