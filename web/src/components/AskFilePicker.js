import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Alert, Button, Checkbox, Modal, Select, Space, Spin, TreeSelect, Typography } from 'antd';
import Input from './IMEInput';
import { getRoot, getChildren } from '../services/drive';
import { listAskableFiles } from '../services/ai';

// Selection lives independently of the current result page and search query.
export default function AskFilePicker({ open, selected, onApply, onCancel }) {
  const [draft, setDraft] = useState(new Map());
  const [query, setQuery] = useState('');
  const [folder, setFolder] = useState('');
  const [folders, setFolders] = useState([]);
  const [mime, setMime] = useState('');
  const [items, setItems] = useState([]);
  const [page, setPage] = useState({});
  const [incomplete, setIncomplete] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const controller = useRef(null);
  const load = useCallback(async (cursor = '') => {
    controller.current?.abort();
    const current = new AbortController();
    controller.current = current;
    setLoading(true); setError('');
    try {
      const result = await listAskableFiles({ q: query, folder_id: folder, mime_types: mime ? [mime] : [], cursor, signal: current.signal });
      if (current.signal.aborted) return;
      setItems(previous => cursor ? [...new Map([...previous, ...(result.items || [])].map(item => [item.id, item])).values()] : (result.items || []));
      setPage(result.page || {}); setIncomplete(Boolean(result.scan_incomplete));
    } catch (value) {
      if (value.name !== 'AbortError' && !current.signal.aborted) setError(value.message || '无法读取可问答文件');
    } finally { if (!current.signal.aborted) setLoading(false); }
  }, [query, folder, mime]);
  useEffect(() => { if (open) { setDraft(new Map(selected.map(file => [file.id, file]))); } }, [open, selected]);
  useEffect(() => {
    if (!open) return undefined;
    const timer = setTimeout(() => { setItems([]); setPage({}); load(); }, 250);
    return () => { clearTimeout(timer); controller.current?.abort(); };
  }, [open, load]);
  useEffect(() => {
    if (!open) return undefined;
    const abort = new AbortController();
    getRoot({ signal: abort.signal }).then(root => { if (!abort.signal.aborted) setFolders([{ value: root.id, title: '根目录', key: root.id }]); }).catch(() => {});
    return () => abort.abort();
  }, [open]);
  const loadFolders = async node => {
    const children = [];
    let cursor;
    do {
      const result = await getChildren(node.value, { limit: 100, cursor });
      children.push(...(result.items || []).filter(item => item.type === 'folder').map(item => ({ value: item.id, key: item.id, title: item.name })));
      cursor = result.page?.has_more ? result.page.next_cursor : '';
    } while (cursor);
    const attach = nodes => nodes.map(item => item.value === node.value ? { ...item, children } : item.children ? { ...item, children: attach(item.children) } : item);
    setFolders(previous => attach(previous));
  };
  const toggle = file => setDraft(previous => {
    const next = new Map(previous);
    if (next.has(file.id)) next.delete(file.id);
    else if (next.size < 50) next.set(file.id, file);
    return next;
  });
  return <Modal open={open} title="选择问答文件" onCancel={onCancel} onOk={() => onApply([...draft.values()])} okText="应用选择" cancelText="取消" okButtonProps={{ disabled: draft.size === 0 }} width={680} destroyOnClose>
    <Space direction="vertical" style={{ width: '100%' }} size="middle">
      <Input aria-label="搜索可问答文件" placeholder="搜索文件名" value={query} onChange={event => setQuery(event.target.value)} />
      <Space wrap>
        <TreeSelect aria-label="按目录筛选" placeholder="全部目录" allowClear value={folder || undefined} onChange={value => setFolder(value || '')} style={{ width: 180 }} treeData={folders} loadData={loadFolders} />
        <Select aria-label="文件类型" value={mime} onChange={setMime} style={{ width: 140 }} options={[{ value: '', label: '全部类型' }, { value: 'application/pdf', label: 'PDF' }, { value: 'text/plain', label: '文本' }, { value: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document', label: 'Word 文档' }]} />
      </Space>
      <Typography.Text>已选 {draft.size} / 50 <Button type="link" onClick={() => setDraft(new Map())}>清空草稿</Button></Typography.Text>
      {draft.size === 50 && <Alert type="info" message="最多选择 50 个文件" />}
      {error && <Alert type="error" message={error} action={<Button onClick={() => load()}>重试</Button>} />}
      <div style={{ maxHeight: 360, overflow: 'auto' }} aria-label="可问答文件">
        {items.map(file => <div key={file.id} style={{ padding: '12px 0', borderBottom: '1px solid var(--divider)' }}>
          <Checkbox checked={draft.has(file.id)} disabled={!draft.has(file.id) && draft.size >= 50} onChange={() => toggle(file)}>
            <span>{file.name}</span><br /><Typography.Text type="secondary">{file.path || file.name}</Typography.Text>
          </Checkbox>
        </div>)}
        {!loading && !error && items.length === 0 && <Typography.Text type="secondary">{page.has_more ? '仍在查找可问答文件，请继续加载' : query || folder || mime ? '没有匹配的可问答文件' : '暂无可问答文件，上传的文档正在准备后会出现在这里'}</Typography.Text>}
      </div>
      {loading && <Spin aria-label="正在加载文件" />}
      {incomplete && <Typography.Text type="secondary">还有文件尚未扫描，可继续加载。</Typography.Text>}
      {page.has_more && <Button loading={loading} onClick={() => load(page.next_cursor)}>加载更多</Button>}
    </Space>
  </Modal>;
}
