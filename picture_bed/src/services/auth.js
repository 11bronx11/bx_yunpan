import { api, restoreSession, setAccessToken } from './api';

export const loginUser = async (login, password) => {
  const session = await api('/api/v1/auth/login', {
    method: 'POST',
    body: JSON.stringify({ login, password }),
  }, false);
  setAccessToken(session.access_token);
  return session;
};

export const registerUser = async values => {
  const session = await api('/api/v1/auth/register', {
    method: 'POST',
    body: JSON.stringify({
      username: values.username,
      email: values.email || '',
      password: values.password,
    }),
  }, false);
  setAccessToken(session.access_token);
  return session;
};

export const restoreUserSession = restoreSession;

export const logoutUser = async () => {
  try {
    await api('/api/v1/auth/logout', { method: 'POST' });
  } finally {
    setAccessToken(null);
  }
};
