import React from 'react';
import { act, render, screen } from '@testing-library/react';
import { AuthProvider, useAuth } from './AuthContext';
import { onSessionExpired } from '../services/api';
import { restoreUserSession } from '../services/auth';

jest.mock('../services/api', () => ({ onSessionExpired: jest.fn() }));
jest.mock('../services/auth', () => ({ restoreUserSession: jest.fn(), logoutUser: jest.fn() }));

test('clears the current user when refresh confirms the session has expired', async () => {
  let expire;
  const unsubscribe = jest.fn();
  onSessionExpired.mockImplementation(listener => { expire = listener; return unsubscribe; });
  restoreUserSession.mockResolvedValue({ user: { id: 'user-1', username: 'tester' } });
  const Probe = () => { const { user, ready } = useAuth(); return <span>{!ready ? 'loading' : user?.username || 'signed out'}</span>; };
  const view = render(<AuthProvider><Probe /></AuthProvider>);
  expect(await screen.findByText('tester')).toBeInTheDocument();
  act(() => expire());
  expect(screen.getByText('signed out')).toBeInTheDocument();
  view.unmount();
  expect(unsubscribe).toHaveBeenCalledTimes(1);
});
