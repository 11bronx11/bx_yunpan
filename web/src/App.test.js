import { fireEvent, render, screen } from '@testing-library/react';
import App from './App';

jest.mock('./services/auth', () => ({
  loginUser: jest.fn(),
  logoutUser: () => Promise.resolve(),
  registerUser: jest.fn(),
  restoreUserSession: () => Promise.reject(new Error('no session')),
}));

jest.mock('./services/drive', () => ({
  createFolder: jest.fn(),
  deleteFile: jest.fn(),
  deleteFolder: jest.fn(),
  getBreadcrumb: jest.fn(),
  getChildren: jest.fn(),
  getDownloadURL: jest.fn(),
  getPreview: jest.fn(),
  getRoot: jest.fn(),
  renameFile: jest.fn(),
  abortUpload: jest.fn(),
  listActiveUploads: jest.fn(),
  uploadFile: jest.fn(),
  waitForUpload: jest.fn(),
}));

test('renders the authentication poster and switches to registration', async () => {
  window.scrollTo = jest.fn();
  render(<App />);
  expect(await screen.findByRole('heading', { name: /your files/i })).toBeInTheDocument();
  expect(screen.getByRole('heading', { name: /welcome back/i })).toBeInTheDocument();

  fireEvent.click(screen.getByRole('button', { name: /create account/i }));

  expect(screen.getByRole('heading', { name: /create your space/i })).toBeInTheDocument();
  expect(screen.getByLabelText(/email/i)).toBeInTheDocument();
});
