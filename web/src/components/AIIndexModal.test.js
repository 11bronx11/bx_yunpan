import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
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

  expect((await screen.findAllByText('构建失败')).length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));

  await waitFor(() => expect(reprocessFileAI).toHaveBeenCalledWith('file-1'));
  expect(await screen.findByText('已索引', {}, { timeout: 3000 })).toBeInTheDocument();
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

  expect((await screen.findAllByText('构建失败')).length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));
  await waitFor(() => expect(getAIJob).toHaveBeenCalledTimes(1));
  const signal = getAIJob.mock.calls[0][1].signal;

  rerender(renderModal(false));
  expect(signal.aborted).toBe(true);
  await act(async () => { finishRequest({ id: 'task-close', status: 'succeeded' }); });
  await waitFor(() => expect(onStatusChange).toHaveBeenCalledTimes(2));
  expect(getFileAI).toHaveBeenCalledTimes(1);
});

test('a canceled task stops polling and allows manual refresh', async () => {
  getFileAI.mockResolvedValue({ status: 'failed', tags: [] });
  reprocessFileAI.mockResolvedValue({ id: 'task-cancel', status: 'pending' });
  getAIJob.mockResolvedValue({ id: 'task-cancel', status: 'canceled' });
  render(<App><AIIndexModal file={{ id: 'file-cancel' }} open onClose={jest.fn()} /></App>);

  expect((await screen.findAllByText('构建失败')).length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));
  expect(await screen.findByText('构建已取消')).toBeInTheDocument();
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
  expect((await screen.findAllByText('构建失败')).length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));
  await screen.findByRole('progressbar');
  const advance = async milliseconds => {
    await act(async () => { jest.advanceTimersByTime(milliseconds); });
  };

  await advance(600);
  expect(getAIJob).toHaveBeenCalledTimes(1);
  await advance(2000);
  expect(getAIJob).toHaveBeenCalledTimes(2);
  expect(screen.getByText('索引状态获取失败，正在重试。')).toBeInTheDocument();
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

test('keeps model metadata while hiding known and unknown index errors', async () => {
  getFileAI.mockResolvedValue({
    status: 'unsupported', error_code: 'ai.vision.rejected',
    model_version: 'qwen-plus+embedding', pipeline_version: 'document-index-v5', tags: [],
  });
  render(<App><AIIndexModal file={{ id: 'image' }} open onClose={jest.fn()} /></App>);

  expect(await screen.findByText('qwen-plus+embedding')).toBeInTheDocument();
  expect(screen.getByText('document-index-v5')).toBeInTheDocument();
  expect(within(screen.getByRole('alert')).getByText('构建失败')).toBeInTheDocument();
  expect(screen.queryByText(/ai\.vision|Provider|AI_MAX|Worker|不支持/)).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: '重新构建索引' })).toBeEnabled();
});

test.each([
  ['failed', 'failed', 'ai.reprocess_failed'],
  ['failed', 'indexed', 'unknown.provider.error'],
  ['succeeded', 'unsupported', 'ai.vision.rejected'],
])('build outcome %s with index %s never leaks provider details or shows success', async (taskStatus, indexStatus, code) => {
  getFileAI
    .mockResolvedValueOnce({ status: 'failed', tags: [] })
    .mockResolvedValueOnce({ status: indexStatus, error_code: code, tags: [] });
  reprocessFileAI.mockResolvedValue({ id: 'private-error', status: 'pending', progress: 0 });
  getAIJob.mockResolvedValue({
    id: 'private-error', status: taskStatus, progress: 100, attempt: 1,
    error_code: code, error_message: 'HTTP 400 AllocationQuota.FreeTierOnly ai.vision.rejected',
  });
  render(<App><AIIndexModal file={{ id: 'private-error' }} open onClose={jest.fn()} /></App>);
  await screen.findByRole('alert');
  const button = screen.getByRole('button', { name: '重新构建索引' });
  fireEvent.click(button);
  fireEvent.click(button);

  expect(await screen.findByText('索引构建请求已提交')).toBeInTheDocument();
  expect(screen.getByText('正在构建')).toBeInTheDocument();
  expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  expect(reprocessFileAI).toHaveBeenCalledTimes(1);
  await waitFor(() => expect(screen.getByRole('progressbar')).toHaveClass('ant-progress-status-exception'));
  expect(screen.getByRole('progressbar')).not.toHaveClass('ant-progress-status-success');
  expect(screen.queryByText(/HTTP 400|AllocationQuota|ai\.vision|unknown\.provider|第 1 次处理|处理已完成/)).not.toBeInTheDocument();
  expect(screen.queryByText('索引构建完成')).not.toBeInTheDocument();
  expect(within(screen.getByRole('alert')).getByText('构建失败')).toBeInTheDocument();
  expect(button).toBeEnabled();
});

test('an unconfirmed result does not claim indexed and can be confirmed by refreshing', async () => {
  getFileAI
    .mockResolvedValueOnce({ status: 'indexed', tags: [] })
    .mockRejectedValueOnce(new Error('upstream HTTP 403 provider details'))
    .mockResolvedValueOnce({ status: 'indexed', tags: [] });
  reprocessFileAI.mockResolvedValue({ id: 'unconfirmed', status: 'pending' });
  getAIJob.mockResolvedValue({ id: 'unconfirmed', status: 'succeeded', progress: 100 });
  render(<App><AIIndexModal file={{ id: 'unconfirmed' }} open onClose={jest.fn()} /></App>);
  await screen.findByText('已索引');
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));

  expect(await screen.findByText('未建立')).toBeInTheDocument();
  expect(screen.getByText('索引状态加载失败，请稍后重试。')).toBeInTheDocument();
  expect(screen.queryByText(/upstream|HTTP 403/)).not.toBeInTheDocument();
  expect(screen.getByRole('progressbar')).not.toHaveClass('ant-progress-status-success');
  fireEvent.click(screen.getByRole('button', { name: '刷新 AI 索引状态' }));
  expect(await screen.findByText('已索引')).toBeInTheDocument();
  expect(screen.getByRole('progressbar')).toHaveClass('ant-progress-status-success');
});

test('submission failures hide provider details', async () => {
  getFileAI.mockResolvedValue({ status: 'indexed', tags: [] });
  reprocessFileAI.mockRejectedValue(new Error('HTTP 400 AllocationQuota.FreeTierOnly'));
  render(<App><AIIndexModal file={{ id: 'rejected' }} open onClose={jest.fn()} /></App>);
  await screen.findByText('已索引');
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));

  expect(await screen.findByText('构建失败')).toBeInTheDocument();
  expect(screen.queryByText(/HTTP 400|AllocationQuota/)).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: '重新构建索引' })).toBeEnabled();
});

test('switching files ignores a late submission response and leaves the next file usable', async () => {
  getFileAI.mockResolvedValue({ status: 'indexed', tags: [] });
  let finishSubmission;
  reprocessFileAI.mockImplementation(() => new Promise(resolve => { finishSubmission = resolve; }));
  const modal = id => <App><AIIndexModal file={{ id, name: id }} open onClose={jest.fn()} /></App>;
  const { rerender } = render(modal('old-file'));
  await screen.findByText('已索引');
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));

  rerender(modal('next-file'));
  await screen.findByText('已索引');
  await act(async () => { finishSubmission({ id: 'old-task', status: 'pending' }); });
  expect(screen.queryByText('索引构建请求已提交')).not.toBeInTheDocument();
  expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: '重新构建索引' })).toBeEnabled();
  expect(getAIJob).not.toHaveBeenCalled();
});

test('a missing ai_status permits building instead of remaining pending forever', async () => {
  getFileAI.mockResolvedValue({ ai_status: 'missing', status: 'pending', tags: [] });
  render(<App><AIIndexModal file={{ id: 'not-built' }} open onClose={jest.fn()} /></App>);

  expect(await screen.findByText('未建立')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: '构建索引' })).toBeEnabled();
});

test.each(['partial_success', 'needs_attention'])('%s refreshes the result, shows failure and allows retry', async status => {
  getFileAI
    .mockResolvedValueOnce({ ai_status: 'failed', tags: [] })
    .mockResolvedValueOnce({ ai_status: 'failed', tags: [] });
  reprocessFileAI.mockResolvedValue({ id: 'partial', status: 'pending' });
  getAIJob.mockResolvedValue({ id: 'partial', status, progress: 100 });
  const onStatusChange = jest.fn();
  render(<App><AIIndexModal file={{ id: 'partial' }} open onClose={jest.fn()} onStatusChange={onStatusChange} /></App>);

  await screen.findByRole('alert');
  fireEvent.click(screen.getByRole('button', { name: '重新构建索引' }));
  await waitFor(() => expect(getFileAI).toHaveBeenCalledTimes(2));
  expect(screen.getByRole('progressbar')).toHaveClass('ant-progress-status-exception');
  expect(screen.getByRole('button', { name: '重新构建索引' })).toBeEnabled();
  expect(onStatusChange).toHaveBeenLastCalledWith('partial', 'failed');
  expect(getAIJob).toHaveBeenCalledTimes(1);
});
