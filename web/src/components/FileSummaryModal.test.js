import { fireEvent, render, screen } from '@testing-library/react';
import { App } from 'antd';
import FileSummaryModal, { AI_SUMMARY_LIMIT, truncateSummary } from './FileSummaryModal';
import { getFileAI } from '../services/ai';

jest.mock('../services/ai', () => ({ getFileAI: jest.fn() }));

test('loads and displays a bounded AI summary', async () => {
  const longSummary = '项'.repeat(AI_SUMMARY_LIMIT + 20);
  getFileAI.mockResolvedValue({ status: 'indexed', summary: longSummary, language: 'zh', tags: ['复盘'] });

  render(<App><FileSummaryModal file={{ id: 'file-1', name: '项目复盘.pdf' }} open onClose={jest.fn()} /></App>);

  expect(await screen.findByText(truncateSummary(longSummary))).toBeInTheDocument();
  expect(getFileAI).toHaveBeenCalledWith('file-1', expect.objectContaining({ signal: expect.any(AbortSignal) }));
  expect(Array.from(truncateSummary(longSummary))).toHaveLength(AI_SUMMARY_LIMIT);
});

test('offers index management when the summary is missing', async () => {
  getFileAI.mockRejectedValue({ status: 404 });
  const onManageIndex = jest.fn();

  render(<App><FileSummaryModal file={{ id: 'file-2', name: '记录.txt' }} open onClose={jest.fn()} onManageIndex={onManageIndex} /></App>);

  expect(await screen.findByText('该文档还没有建立 AI 摘要。')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '管理索引' }));
  expect(onManageIndex).toHaveBeenCalledWith({ id: 'file-2', name: '记录.txt' });
});
