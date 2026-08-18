const { createProxyMiddleware } = require('http-proxy-middleware');

module.exports = app => {
  app.use('/api', createProxyMiddleware({
    target: process.env.YUNPAN_LOCAL_API_URL || 'http://127.0.0.1:8081',
    changeOrigin: false,
  }));
  app.use('/storage', createProxyMiddleware({
    target: process.env.YUNPAN_LOCAL_STORAGE_URL || 'http://127.0.0.1:9000',
    changeOrigin: false,
    pathRewrite: { '^/storage': '' },
  }));
};
