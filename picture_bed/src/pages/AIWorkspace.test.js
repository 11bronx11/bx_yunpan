import React from 'react';
import { App } from 'antd';
import { fireEvent, render, screen } from '@testing-library/react';
import AIWorkspace from './AIWorkspace';
import { searchFiles } from '../services/ai';

jest.mock('../services/ai', () => ({ askFiles: jest.fn(), searchFiles: jest.fn() }));
jest.mock('../components/SearchResultModal', () => ({ hit, open }) => open ? <div role="dialog">已打开 {hit.file.name}</div> : null);

test('opens a search result with the keyboard', async () => {
  searchFiles.mockResolvedValue({
    hits: [{
      file: { id: 'file-1', name: 'report.pdf', version: 1 },
      match_type: 'hybrid',
      score: 0.016,
      citations: [],
    }],
  });
  render(<App><AIWorkspace /></App>);

  fireEvent.change(screen.getByPlaceholderText('例如：上季度项目复盘中的风险'), { target: { value: '项目风险' } });
  fireEvent.click(screen.getByRole('button', { name: /搜索$/ }));
  const result = await screen.findByRole('button', { name: '打开 report.pdf 文件详情' });
  fireEvent.keyDown(result, { key: 'Enter' });

  expect(screen.getByRole('dialog')).toHaveTextContent('已打开 report.pdf');
});
