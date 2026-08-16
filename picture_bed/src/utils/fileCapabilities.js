const summaryMimeTypes = new Set([
  'text/plain',
  'text/markdown',
  'text/x-markdown',
  'application/json',
  'text/csv',
  'application/pdf',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'text/x-c',
  'text/x-c++',
  'text/x-go',
  'text/x-java-source',
  'text/x-python',
  'application/javascript',
  'text/javascript',
  'application/typescript',
  'text/typescript',
  'application/sql',
  'application/xml',
  'text/xml',
  'application/x-sh',
]);

const sourceFileNames = new Set([
  'dockerfile',
  'makefile',
  'cmakelists.txt',
  'go.mod',
  'go.sum',
  'package.json',
  'package-lock.json',
]);

const sourceExtensions = new Set([
  '.c', '.cc', '.cpp', '.cxx', '.h', '.hh', '.hpp', '.hxx',
  '.go', '.java', '.kt', '.kts', '.py', '.rb', '.php', '.rs', '.swift',
  '.js', '.jsx', '.mjs', '.cjs', '.ts', '.tsx', '.vue', '.svelte',
  '.sh', '.bash', '.zsh', '.fish', '.ps1', '.sql', '.graphql',
  '.html', '.htm', '.css', '.scss', '.less', '.xml',
  '.yaml', '.yml', '.toml', '.ini', '.conf', '.properties', '.env',
]);

const normalizedMimeType = file => (file?.mime_type || '').toLowerCase().split(';')[0].trim();

const isSourceFileName = value => {
  const name = String(value || '').trim().toLowerCase().split(/[\\/]/).pop();
  if (sourceFileNames.has(name)) return true;
  const dot = name.lastIndexOf('.');
  return dot >= 0 && sourceExtensions.has(name.slice(dot));
};

export const supportsAISummary = file => summaryMimeTypes.has(normalizedMimeType(file)) || isSourceFileName(file?.name);
