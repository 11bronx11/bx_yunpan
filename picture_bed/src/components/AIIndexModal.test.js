import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { App } from 'antd';
import AIIndexModal from './AIIndexModal';
import { getAIJob, getFileAI, reprocessFileAI } from '../services/ai';

jest.mock('../services/ai', () => ({
  getAIJob: jest.fn(),
  getFileAI: jest.fn(),
  reprocessFileAI: jest.fn(),
}));

test('reprocesses a failed AI index and refreshes its status', async () => {
  getFileAI
    .mockResolvedValueOnce({ status: 'failed', error_code: 'ai.embedding_failed', model_version: 'old-model', pipeline_version: 'document-index-v2', tags: [] })
    .mockResolvedValueOnce({ status: 'indexed', model_version: 'new-model', pipeline_version: 'document-index-v2', tags: ['report'] });
  reprocessFileAI.mockResolvedValue({ id: 'task-1', status: 'pending', progress: 0, attempt: 0 });
  getAIJob.mockResolvedValue({ id: 'task-1', status: 'succeeded', progress: 100, attempt: 1 });
  const onStatusChange = jest.fn();

  render(<App><AIIndexModal file={{ id: 'file-1', name: 'report.pdf' }} open onClose={jest.fn()} onStatusChange={onStatusChange} /></App>);

  expect(await screen.findByText('构建失败')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));

  await waitFor(() => expect(reprocessFileAI).toHaveBeenCalledWith('file-1'));
  expect(await screen.findByText('已完成', {}, { timeout: 3000 })).toBeInTheDocument();
  expect(getAIJob).toHaveBeenCalledWith('task-1');
  expect(onStatusChange).toHaveBeenLastCalledWith('file-1', 'indexed');
});
