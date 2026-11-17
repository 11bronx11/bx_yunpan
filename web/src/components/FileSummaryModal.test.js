import { fireEvent, render, screen } from '@testing-library/react';
import { App } from 'antd';
import FileSummaryModal, { AI_SUMMARY_LIMIT, truncateSummary } from './FileSummaryModal';
import { getFileAI } from '../services/ai';

jest.mock('../services/ai', () => ({ getFileAI: jest.fn() }));

test('displays a bounded read-only overview with only the header close control', async () => {
  const longSummary = '项'.repeat(AI_SUMMARY_LIMIT + 20);
  getFileAI.mockResolvedValue({ status: 'indexed', summary: longSummary, language: 'zh', tags: ['复盘'] });

  const onClose = jest.fn();
  render(<App><FileSummaryModal file={{ id: 'file-1', name: '项目复盘.pdf' }} open onClose={onClose} /></App>);

  expect(await screen.findByText(truncateSummary(longSummary))).toBeInTheDocument();
  expect(getFileAI).toHaveBeenCalledWith('file-1', expect.objectContaining({ signal: expect.any(AbortSignal) }));
  expect(Array.from(truncateSummary(longSummary))).toHaveLength(AI_SUMMARY_LIMIT);
  expect(screen.getByRole('article', { name: 'AI 概览' })).toHaveTextContent('复盘');
  expect(screen.getAllByRole('button')).toHaveLength(1);
  fireEvent.click(screen.getByRole('button', { name: 'Close' }));
  expect(onClose).toHaveBeenCalledTimes(1);
});

test('shows a plain unavailable message without index management when the summary is missing', async () => {
  getFileAI.mockRejectedValue({ status: 404 });

  render(<App><FileSummaryModal file={{ id: 'file-2', name: '记录.txt' }} open onClose={jest.fn()} /></App>);

  expect(await screen.findByText('暂时无法查看AI摘要')).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: '管理索引' })).not.toBeInTheDocument();
});

test.each(['failed', 'unsupported', 'pending', 'processing', 'indexed'])('unavailable %s summaries do not expose index or provider details', async status => {
  getFileAI.mockResolvedValue({ status, summary: '', error_code: 'ai.vision.rejected' });
  render(<App><FileSummaryModal file={{ id: 'file-3', name: '记录.txt' }} open onClose={jest.fn()} /></App>);

  expect(await screen.findByText('暂时无法查看AI摘要')).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: '管理索引' })).not.toBeInTheDocument();
  expect(screen.queryByText(/ai\.vision|重建索引|构建失败|索引队列/)).not.toBeInTheDocument();
});
