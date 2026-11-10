import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Alert, App, Button, Descriptions, Modal, Space, Table, Tabs } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';
import { listShares, revokeShare } from '../services/share';
import { GraphicEmpty, PageHeader, ProductPage } from '../components/design/ProductUI';
import ShareModal, { formatShareExpiry } from '../components/ShareModal';

const PAGE_SIZE = 10;
const statusMeta = {
  active: { label: '有效', tone: 'mint' }, expired: { label: '已过期', tone: 'coral' },
  revoked: { label: '已撤销', tone: 'coral' }, source_deleted: { label: '源文件已删除', tone: 'coral' },
};
const tabs = [
  { key: 'active', label: '分享有效' }, { key: 'expired', label: '分享过期' }, { key: 'revoked', label: '分享撤销' }, { key: 'source_deleted', label: '源文件已删除' },
];
const fileType = share => {
  const extension = share.file_name?.match(/\.([a-z0-9]{1,12})$/i)?.[1];
  if (extension) return extension.toUpperCase();
  const mime = share.mime_type || '';
  if (mime.startsWith('image/')) return '图片';
  if (mime.startsWith('video/')) return '视频';
  if (mime.startsWith('audio/')) return '音频';
  if (mime.startsWith('text/')) return '文本';
  return mime === 'application/pdf' ? 'PDF' : '文件';
};
export const shareStatus = (share, now = Date.now()) => {
  if (share.source_deleted_at || share.status === 'source_deleted') return 'source_deleted';
  if (share.revoked_at || share.status === 'revoked') return 'revoked';
  if (share.status === 'expired' || (share.expires_at && new Date(share.expires_at).valueOf() <= now)) return 'expired';
  return 'active';
};

export default function Shares() {
  const { message, modal } = App.useApp();
  const [items, setItems] = useState([]);
  const [page, setPage] = useState({});
  const [query, setQuery] = useState({ status: 'active', cursors: [''], index: 0 });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [editing, setEditing] = useState(null);
  const [viewing, setViewing] = useState(null);
  const [now, setNow] = useState(Date.now());
  const request = useRef(null);
  const load = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController(); request.current = controller;
    setLoading(true); setError('');
    try {
      const result = await listShares({ status: query.status, cursor: query.cursors[query.index], limit: PAGE_SIZE, signal: controller.signal });
      if (controller.signal.aborted) return;
      setItems(result.items); setPage(result.page || {}); setNow(Date.now());
    } catch (err) { if (!controller.signal.aborted) setError(err.message || '分享列表加载失败'); }
    finally { if (!controller.signal.aborted) setLoading(false); }
  }, [query]);
  useEffect(() => { load(); return () => request.current?.abort(); }, [load]);
  const refresh = useCallback(() => {
    setItems([]); setPage({});
    setQuery(current => ({ status: current.status, cursors: [''], index: 0 }));
  }, []);
  useEffect(() => {
    if (query.status !== 'active') return;
    const future = items.map(item => new Date(item.expires_at).valueOf()).filter(value => value > Date.now());
    if (!future.length) return;
    const timer = setTimeout(refresh, Math.min(Math.min(...future) - Date.now() + 50, 2147483647));
    return () => clearTimeout(timer);
  }, [items, query.status, refresh]);
  const changeTab = status => {
    request.current?.abort(); setItems([]); setPage({}); setError(''); setEditing(null);
    setQuery({ status, cursors: [''], index: 0 });
  };
  const turnPage = direction => {
    setItems([]); setPage({});
    setQuery(current => {
      const cursors = direction > 0 ? [...current.cursors.slice(0, current.index + 1), page.next_cursor] : current.cursors;
      return { ...current, cursors, index: current.index + direction };
    });
  };
  const revoke = share => modal.confirm({
    title: '提前撤销分享？', content: '撤销后原 Key 将失效，已经导入的文件不受影响。此操作不能恢复。',
    okText: '撤销分享', cancelText: '保留分享', okButtonProps: { danger: true },
    onOk: async () => {
      try { await revokeShare(share.id); message.success('分享已撤销'); refresh(); }
      catch (err) { message.error(err.message || '撤销分享失败'); refresh(); throw err; }
    },
  });
  const columns = [
    { title: '文件', dataIndex: 'file_name', render: name => <span style={{ overflowWrap: 'anywhere' }}>{name || '未知文件'}</span> },
    { title: '类型', dataIndex: 'mime_type', width: 100, render: (_, share) => fileType(share) },
    ...(query.status === 'active' ? [{ title: '有效期', dataIndex: 'expires_at', width: 210, render: formatShareExpiry }] : []),
    { title: '操作', key: 'actions', fixed: 'right', width: query.status === 'active' ? 280 : 90, render: (_, share) => <Space>
      <Button size="small" onClick={() => setViewing(share)}>查看</Button>
      {shareStatus(share, now) === 'active' && <>
        <Button size="small" onClick={() => setEditing(share)}>修改有效期</Button>
        <Button size="small" danger onClick={() => revoke(share)}>提前撤销</Button>
      </>}
    </Space> },
  ];
  return <ProductPage page="shares">
    <PageHeader title="我的分享" action={<Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>刷新</Button>} />
    <Tabs activeKey={query.status} items={tabs} onChange={changeTab} />
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {error && <Alert type="error" showIcon message={error} action={<Button onClick={load}>重试</Button>} />}
      <Table rowKey="id" columns={columns} dataSource={items} pagination={false} loading={loading} scroll={{ x: query.status === 'active' ? 900 : 480 }} locale={{ emptyText: <GraphicEmpty title={`暂无${query.status === 'source_deleted' ? '源文件已删除的' : tabs.find(tab => tab.key === query.status).label.slice(2)}分享`} description={query.status === 'active' ? '选择文件即可创建分享。' : ''} /> }} />
      <Space aria-label="分享分页" wrap>
        <Button disabled={loading || query.index === 0} onClick={() => turnPage(-1)}>上一页</Button>
        <span>第 {query.index + 1} 页 · 每页 {PAGE_SIZE} 条</span>
        <Button disabled={loading || !page.has_more || Boolean(error)} onClick={() => turnPage(1)}>下一页</Button>
      </Space>
    </Space>
    <ShareModal open={Boolean(editing)} share={editing} onCancel={() => setEditing(null)} onSaved={refresh} />
    <Modal open={Boolean(viewing)} title="分享详情" onCancel={() => setViewing(null)} footer={<Button onClick={() => setViewing(null)}>关闭</Button>}>
      {viewing && <Descriptions column={1} items={[
        { key: 'file', label: '文件', children: viewing.file_name || '未知文件' },
        { key: 'type', label: '类型', children: fileType(viewing) },
        { key: 'status', label: '状态', children: statusMeta[shareStatus(viewing, now)].label },
      ]} />}
    </Modal>
  </ProductPage>;
}
