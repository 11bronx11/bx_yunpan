import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Alert, Button, Checkbox, Modal, Spin, Tooltip, TreeSelect, Typography } from 'antd';
import { ClearOutlined, FileOutlined, SearchOutlined } from '@ant-design/icons';
import Input from './IMEInput';
import { getRoot, getChildren } from '../services/drive';
import { listAskableFiles } from '../services/ai';
import './AskFilePicker.css';

// Selection lives independently of the current result page and search query.
export default function AskFilePicker({ open, selected, onApply, onCancel }) {
  const [draft, setDraft] = useState(new Map());
  const [query, setQuery] = useState('');
  const [folder, setFolder] = useState('');
  const [folders, setFolders] = useState([]);
  const [expandedFolders, setExpandedFolders] = useState([]);
  const [directoryError, setDirectoryError] = useState('');
  const [items, setItems] = useState([]);
  const [page, setPage] = useState({});
  const [incomplete, setIncomplete] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const controller = useRef(null);
  const directoryController = useRef(null);
  const directoryRequests = useRef(new Map());
  const results = useRef(new Map());
  const previousQuery = useRef('');
  const searchInput = useRef(null);
  const load = useCallback(async (cursor = '') => {
    controller.current?.abort();
    const current = new AbortController();
    controller.current = current;
    const key = JSON.stringify([query, folder]);
    const cached = results.current.get(key);
    if (!cursor && cached) {
      setItems(cached.items); setPage(cached.page); setIncomplete(cached.incomplete);
      setLoading(false); setError('');
      return;
    }
    setLoading(true); setError('');
    try {
      const result = await listAskableFiles({ q: query, folder_id: folder, include_subfolders: !folder, cursor, signal: current.signal });
      if (current.signal.aborted) return;
      const next = {
        items: cursor ? [...new Map([...(cached?.items || []), ...(result.items || [])].map(item => [item.id, item])).values()] : (result.items || []),
        page: result.page || {}, incomplete: Boolean(result.scan_incomplete),
      };
      results.current.delete(key);
      results.current.set(key, next);
      if (results.current.size > 10) results.current.delete(results.current.keys().next().value);
      setItems(next.items); setPage(next.page); setIncomplete(next.incomplete);
    } catch (value) {
      if (value.name !== 'AbortError' && !current.signal.aborted) {
        if (!cursor) { setItems([]); setPage({}); setIncomplete(false); }
        setError(value.message || '无法读取可问答文件');
      }
    } finally { if (!current.signal.aborted) setLoading(false); }
  }, [query, folder]);
  useEffect(() => { if (open) { setDraft(new Map(selected.map(file => [file.id, file]))); } }, [open, selected]);
  useEffect(() => {
    if (open) {
      results.current.clear();
      setQuery(''); setFolder(''); setItems([]); setPage({}); setIncomplete(false);
      previousQuery.current = '';
    }
  }, [open]);
  useEffect(() => {
    if (!open) return undefined;
    const queryChanged = previousQuery.current !== query;
    previousQuery.current = query;
    setLoading(true); setError('');
    const timer = queryChanged && query ? setTimeout(() => load(), 250) : null;
    if (!timer) load();
    return () => { clearTimeout(timer); controller.current?.abort(); };
  }, [open, load, query]);
  const loadFolders = useCallback(node => {
    if (directoryRequests.current.has(node.value)) return directoryRequests.current.get(node.value);
    const abort = directoryController.current;
    const request = (async () => {
      const children = [];
      let cursor;
      do {
        const result = await getChildren(node.value, { limit: 100, cursor, signal: abort.signal });
        if (abort.signal.aborted) return;
        children.push(...(result.items || []).filter(item => item.type === 'folder').map(item => ({ value: item.id, key: item.id, title: item.name })));
        cursor = result.page?.has_more ? result.page.next_cursor : '';
      } while (cursor);
      const attach = nodes => nodes.map(item => item.value === node.value ? { ...item, children, isLeaf: children.length === 0 } : item.children ? { ...item, children: attach(item.children) } : item);
      setFolders(previous => attach(previous));
    })().catch(error => {
      if (!abort.signal.aborted) { directoryRequests.current.delete(node.value); setDirectoryError('目录暂时无法加载，请重新打开选择文件。'); }
      throw error;
    });
    directoryRequests.current.set(node.value, request);
    return request;
  }, []);
  useEffect(() => {
    if (!open) return undefined;
    const abort = new AbortController();
    directoryController.current = abort;
    directoryRequests.current.clear();
    setDirectoryError('');
    getRoot({ signal: abort.signal }).then(root => {
      if (abort.signal.aborted) return;
      const node = { value: root.id, title: '根目录', key: root.id };
      setFolders([node]);
      setExpandedFolders([root.id]);
      return loadFolders(node);
    }).catch(() => { if (!abort.signal.aborted) setDirectoryError('目录暂时无法加载，请重新打开选择文件。'); });
    return () => abort.abort();
  }, [open, loadFolders]);
  const toggle = file => setDraft(previous => {
    const next = new Map(previous);
    if (next.has(file.id)) next.delete(file.id);
    else if (next.size < 50) next.set(file.id, file);
    return next;
  });
  return <Modal rootClassName="ask-file-picker" open={open} title="选择问答文件" onCancel={onCancel} onOk={() => onApply([...draft.values()])} afterOpenChange={visible => { if (visible) searchInput.current?.focus(); }} okText="应用选择" cancelText="取消" width={680} destroyOnClose>
    <div className="ask-picker-body">
      <Input ref={searchInput} aria-label="搜索可问答文件" placeholder="搜索全部目录的文件" prefix={<SearchOutlined />} allowClear value={query} onChange={event => { setQuery(event.target.value); setFolder(''); }} />
      <div className="ask-picker-toolbar">
        <TreeSelect aria-label="按目录筛选" placeholder="全部目录" allowClear value={folder || undefined} onChange={value => setFolder(value || '')} treeData={folders} loadData={loadFolders} treeExpandedKeys={expandedFolders} onTreeExpand={setExpandedFolders} />
        <span className="ask-picker-count" role="status" aria-live="polite" aria-atomic="true">已选 <strong>{draft.size}</strong><span> / 50</span></span>
        <Tooltip title="清空选择">
          <Button className="ask-picker-clear" aria-label="清空选择" icon={<ClearOutlined />} disabled={draft.size === 0} onClick={() => setDraft(new Map())} />
        </Tooltip>
      </div>
      {directoryError && <Alert type="warning" message={directoryError} />}
      {draft.size === 50 && <Alert type="info" message="最多选择 50 个文件" />}
      {error && <Alert type="error" message={error} action={<Button onClick={() => load()}>重试</Button>} />}
      <Spin spinning={loading} delay={120} wrapperClassName="ask-picker-results" tip="正在加载文件">
        <div className="ask-picker-list" aria-label="可问答文件" aria-busy={loading}>
          {items.map(file => <Checkbox key={file.id} className="ask-file-row" checked={draft.has(file.id)} disabled={loading || (!draft.has(file.id) && draft.size >= 50)} onChange={() => toggle(file)}>
            <span className="ask-file-icon" aria-hidden="true"><FileOutlined /></span>
            <span className="ask-file-copy"><span className="ask-file-name" title={file.name}>{file.name}</span>{!folder && <span className="ask-file-path" title={file.path || file.name}>{file.path || file.name}</span>}</span>
          </Checkbox>)}
          {!loading && !error && items.length === 0 && <Typography.Text className="ask-picker-empty" type="secondary">{page.has_more ? '仍在查找可问答文件，请继续加载' : query || folder ? '没有匹配的可问答文件' : '暂无可问答文件，上传的文档正在准备后会出现在这里'}</Typography.Text>}
        </div>
      </Spin>
      {!loading && incomplete && <Typography.Text type="secondary">还有文件尚未扫描，可继续加载。</Typography.Text>}
      {page.has_more && <Button loading={loading} disabled={loading} onClick={() => load(page.next_cursor)}>加载更多</Button>}
    </div>
  </Modal>;
}
