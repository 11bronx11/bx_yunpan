import { supportsAISummary } from './fileCapabilities';

test.each([
  ['main.cpp', 'application/octet-stream'],
  ['server.go', 'text/plain'],
  ['Dockerfile', 'application/octet-stream'],
  ['CMakeLists.txt', 'application/octet-stream'],
  ['app.tsx', 'application/typescript'],
  ['config.yaml', 'application/octet-stream'],
])('supports AI summaries for source file %s', (name, mime_type) => {
  expect(supportsAISummary({ name, mime_type })).toBe(true);
});

test('does not offer an AI summary for an unsupported binary file', () => {
  expect(supportsAISummary({ name: 'archive.zip', mime_type: 'application/zip' })).toBe(false);
});
