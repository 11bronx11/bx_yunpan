import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { App } from 'antd';
import AIIndexModal from './AIIndexModal';
import { getAIJob, getFileAI, reprocessFileAI } from '../services/ai';

jest.mock('../services/ai', () => ({
  getAIJob: jest.fn(),
  getFileAI: jest.fn(),
  reprocessFileAI: jest.fn(),
}));

beforeEach(() => {
  getAIJob.mockReset();
  getFileAI.mockReset();
  reprocessFileAI.mockReset();
});

afterEach(() => jest.useRealTimers());

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
  expect(getAIJob).toHaveBeenCalledWith('task-1', { signal: expect.any(AbortSignal) });
  expect(onStatusChange).toHaveBeenLastCalledWith('file-1', 'indexed');
});

test('closing the modal aborts an in-flight task check and ignores its late result', async () => {
  getFileAI.mockResolvedValue({ status: 'failed', tags: [] });
  reprocessFileAI.mockResolvedValue({ id: 'task-close', status: 'pending' });
  let finishRequest;
  getAIJob.mockImplementation(() => new Promise(resolve => { finishRequest = resolve; }));
  const onStatusChange = jest.fn();
  const renderModal = open => <App><AIIndexModal file={{ id: 'file-close', name: 'report.pdf' }} open={open} onClose={jest.fn()} onStatusChange={onStatusChange} /></App>;
  const { rerender } = render(renderModal(true));

  expect(await screen.findByText('构建失败')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));
  await waitFor(() => expect(getAIJob).toHaveBeenCalledTimes(1));
  const signal = getAIJob.mock.calls[0][1].signal;

  rerender(renderModal(false));
  expect(signal.aborted).toBe(true);
  await act(async () => { finishRequest({ id: 'task-close', status: 'succeeded' }); });
  await waitFor(() => expect(onStatusChange).toHaveBeenCalledTimes(1));
  expect(getFileAI).toHaveBeenCalledTimes(1);
});

test('a canceled task stops polling and allows manual refresh', async () => {
  getFileAI.mockResolvedValue({ status: 'failed', tags: [] });
  reprocessFileAI.mockResolvedValue({ id: 'task-cancel', status: 'pending' });
  getAIJob.mockResolvedValue({ id: 'task-cancel', status: 'canceled' });
  render(<App><AIIndexModal file={{ id: 'file-cancel' }} open onClose={jest.fn()} /></App>);

  expect(await screen.findByText('构建失败')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));
  expect(await screen.findByText('AI 索引任务已取消')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: '刷新 AI 索引状态' })).toBeEnabled();
  expect(getAIJob).toHaveBeenCalledTimes(1);
});

test('task checks back off, recover from temporary failure and speed up on progress', async () => {
  jest.useFakeTimers();
  getFileAI.mockResolvedValue({ status: 'failed', tags: [] });
  reprocessFileAI.mockResolvedValue({ id: 'task-retry', status: 'pending' });
  const pending = { id: 'task-retry', status: 'pending', progress: 0, attempt: 0, poll_after_ms: 2000 };
  getAIJob
    .mockResolvedValueOnce(pending)
    .mockRejectedValueOnce(Object.assign(new Error('服务暂时不可用，请稍后重试'), { status: 503 }))
    .mockResolvedValueOnce(pending)
    .mockResolvedValueOnce({ ...pending, status: 'running', progress: 50, attempt: 1, poll_after_ms: 1000 })
    .mockResolvedValueOnce({ ...pending, status: 'succeeded', progress: 100 });
  render(<App><AIIndexModal file={{ id: 'file-retry' }} open onClose={jest.fn()} /></App>);
  expect(await screen.findByText('构建失败')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));
  await screen.findByRole('progressbar');
  const advance = async milliseconds => {
    await act(async () => { jest.advanceTimersByTime(milliseconds); });
  };

  await advance(600);
  expect(getAIJob).toHaveBeenCalledTimes(1);
  await advance(2000);
  expect(getAIJob).toHaveBeenCalledTimes(2);
  expect(screen.getByText('服务暂时不可用，请稍后重试')).toBeInTheDocument();
  await advance(5000);
  expect(getAIJob).toHaveBeenCalledTimes(3);
  await advance(4000);
  expect(getAIJob).toHaveBeenCalledTimes(4);
  await advance(1000);
  expect(getAIJob).toHaveBeenCalledTimes(5);
  await advance(30000);
  expect(getAIJob).toHaveBeenCalledTimes(5);
  expect(screen.getByRole('button', { name: '刷新 AI 索引状态' })).toBeEnabled();
});
