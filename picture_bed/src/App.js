import React, { useLayoutEffect } from 'react';
import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { Layout, Spin } from 'antd';
import styled from '@emotion/styled';
import Login from './pages/Login';
import Drive from './pages/Drive';
import AIWorkspace from './pages/AIWorkspace';
import AIAsk from './pages/AIAsk';
import Transfers from './pages/Transfers';
import NavBar from './components/NavBar';
import { AuthProvider, useAuth } from './contexts/AuthContext';
import { TransferProvider } from './contexts/TransferContext';

const Shell = styled(Layout)`min-height: 100vh; background: var(--paper);`;
const Content = styled(Layout.Content)`width: min(calc(100% - 48px), 1344px); min-width: 0; margin: 44px auto 80px; @media (max-width: 760px) { width: calc(100% - 28px); margin-top: 32px; }`;

const PrivateRoute = ({ children }) => {
  const { user, ready } = useAuth();
  if (!ready) return <Spin fullscreen />;
  return user ? children : <Navigate to="/login" replace />;
};

const LoginRoute = () => {
  const { user, ready } = useAuth();
  if (!ready) return <Spin fullscreen />;
  return user ? <Navigate to="/drive" replace /> : <Login />;
};

const ScrollReset = () => {
  const { pathname } = useLocation();
  useLayoutEffect(() => {
    window.scrollTo({ top: 0, left: 0, behavior: 'auto' });
  }, [pathname]);
  return null;
};

const AppRoutes = () => <Routes>
  <Route path="/login" element={<LoginRoute />} />
  <Route path="*" element={<PrivateRoute><Shell className="product-shell"><NavBar /><Content><Routes><Route path="/" element={<Navigate to="/drive" replace />} /><Route path="/drive" element={<Drive />} /><Route path="/transfers" element={<Transfers />} /><Route path="/ai" element={<AIWorkspace />} /><Route path="/ask" element={<AIAsk />} /><Route path="*" element={<Navigate to="/drive" replace />} /></Routes></Content></Shell></PrivateRoute>} />
</Routes>;

const App = () => <AuthProvider><TransferProvider><BrowserRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}><ScrollReset /><AppRoutes /></BrowserRouter></TransferProvider></AuthProvider>;
export default App;
