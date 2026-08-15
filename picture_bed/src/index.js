import React from 'react';
import ReactDOM from 'react-dom/client';
import './index.css';
import './styles/product.css';
import App from './App';
import { App as AntApp, ConfigProvider } from 'antd';
import { theme } from './theme';

const root = ReactDOM.createRoot(document.getElementById('root'));
root.render(
    <ConfigProvider theme={theme}>
      <AntApp>
        <App />
      </AntApp>
    </ConfigProvider>
);
