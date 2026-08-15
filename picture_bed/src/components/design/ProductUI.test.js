import React from 'react';
import { render, screen } from '@testing-library/react';
import { FileTypeBadge, fileTypeLabel } from './ProductUI';

test('uses meaningful labels for MIME file types', () => {
  render(<FileTypeBadge type="application/pdf" />);
  expect(screen.getByText('PDF')).toBeInTheDocument();
});

test('uses a folder icon instead of a generic file label', () => {
  render(<FileTypeBadge type="folder" />);
  expect(screen.getByLabelText('目录')).toBeInTheDocument();
  expect(screen.queryByText('FILE')).not.toBeInTheDocument();
});

test('shortens Office MIME types for table cells', () => {
  expect(fileTypeLabel('application/vnd.openxmlformats-officedocument.wordprocessingml.document')).toBe('DOCX');
});
