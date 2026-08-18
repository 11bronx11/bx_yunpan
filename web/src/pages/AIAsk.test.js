import React from 'react';
import { App } from 'antd';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import AIAsk from './AIAsk';
import { askFiles } from '../services/ai';

jest.mock('../services/ai', () => ({ askFiles: jest.fn() }));

test('asks indexed files and renders the answer with citations', async () => {
  askFiles.mockResolvedValue({
    answer: '待跟进事项包括接口联调和上线检查。',
    citations: [{ id: 'citation-1', file_name: '项目复盘.pdf', page_number: 3 }],
  });
  render(<App><AIAsk /></App>);

  fireEvent.change(screen.getByLabelText('向文件提问'), { target: { value: '有哪些待跟进事项？' } });
  await waitFor(() => expect(screen.getByRole('button', { name: '提问' })).toBeVisible());
  fireEvent.click(screen.getByRole('button', { name: '提问' }));

  await waitFor(() => expect(askFiles).toHaveBeenCalledWith('有哪些待跟进事项？'));
  await waitFor(() => expect(screen.getByLabelText('向文件提问')).toHaveValue(''));
  expect(await screen.findByText('待跟进事项包括接口联调和上线检查。')).toBeInTheDocument();
  expect(screen.getByText(/项目复盘\.pdf \/ 第 3 页/)).toBeInTheDocument();
});
