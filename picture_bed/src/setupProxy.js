const { createProxyMiddleware } = require('http-proxy-middleware');

module.exports = app => {
  app.use('/storage', createProxyMiddleware({
    target: 'http://127.0.0.1:9000',
    changeOrigin: false,
    pathRewrite: { '^/storage': '' },
  }));
};
