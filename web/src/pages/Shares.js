import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Alert, App, Button, Space, Table } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';
import { listShares, revokeShare } from '../services/share';
import { GraphicEmpty, PageHeader, ProductPage, StatusTag } from '../components/design/ProductUI';
import ShareModal, { formatShareExpiry } from '../components/ShareModal';

const statusMeta = {
  active: { label: '有效', tone: 'mint' }, expired: { label: '已过期', tone: 'coral' },
  revoked: { label: '已撤销', tone: 'coral' }, unavailable: { label: '源文件失效', tone: 'coral' },
};
export const shareStatus = (share, now = Date.now()) => {
  if (share.revoked_at || share.status === 'revoked') return 'revoked';
  if (share.file_available === false || share.status === 'unavailable') return 'unavailable';
  if (share.expires_at && new Date(share.expires_at).valueOf() <= now) return 'expired';
  return 'active';
};

export default function Shares() {
  const { message, modal } = App.useApp();
  const [items, setItems] = useState([]);
  const [page, setPage] = useState({});
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [editing, setEditing] = useState(null);
  const [now, setNow] = useState(Date.now());
  const request = useRef(null);
  const load = useCallback(async (cursor = '') => {
    request.current?.abort();
    const controller = new AbortController(); request.current = controller;
    setLoading(true); setError('');
    try {
      const result = await listShares({ cursor, limit: 50, signal: controller.signal });
      if (controller.signal.aborted) return;
      setItems(current => cursor ? [...current, ...result.items.filter(item => !current.some(old => old.id === item.id))] : result.items);
      setPage(result.page || {}); setNow(Date.now());
    } catch (err) { if (!controller.signal.aborted) setError(err.message || '分享列表加载失败'); }
    finally { if (!controller.signal.aborted) setLoading(false); }
  }, []);
  useEffect(() => { load(); return () => request.current?.abort(); }, [load]);
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(timer); }, []);
  const revoke = share => modal.confirm({
    title: '取消分享？', content: '取消后原 Key 将失效，已经导入的文件不受影响。此操作不能恢复。',
    okText: '取消分享', cancelText: '保留分享', okButtonProps: { danger: true },
    onOk: async () => {
      try { await revokeShare(share.id); message.success('分享已取消'); await load(); }
      catch (err) { message.error(err.message || '取消分享失败'); throw err; }
    },
  });
  const columns = [
    { title: '文件', dataIndex: 'file_name', render: (name, share) => <span style={{ overflowWrap: 'anywhere' }}>{name || `文件 ${share.file_id}`}</span> },
    { title: '状态', key: 'status', width: 120, render: (_, share) => { const meta = statusMeta[shareStatus(share, now)]; return <StatusTag tone={meta.tone}>{meta.label}</StatusTag>; } },
    { title: '有效期', dataIndex: 'expires_at', width: 210, render: formatShareExpiry },
    { title: '创建时间', dataIndex: 'created_at', width: 210, render: value => new Date(value).toLocaleString() },
    { title: '操作', key: 'actions', width: 220, render: (_, share) => <Space>
      <Button size="small" disabled={Boolean(share.revoked_at) || !share.file_available} onClick={() => setEditing(share)}>修改有效期</Button>
      <Button size="small" danger disabled={Boolean(share.revoked_at)} onClick={() => revoke(share)}>取消分享</Button>
    </Space> },
  ];
  return <ProductPage page="shares">
    <PageHeader title="我的分享" description="查看分享状态，调整有效期，随时取消分享。" action={<Button icon={<ReloadOutlined />} loading={loading} onClick={() => load()}>刷新</Button>} />
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Alert type="info" showIcon message="分享 Key 仅在创建时显示一次。需要新的 Key 时，请从文件菜单重新创建分享。" />
      {error && <Alert type="error" showIcon message={error} action={<Button onClick={() => load()}>重试</Button>} />}
      <Table rowKey="id" columns={columns} dataSource={items} pagination={false} loading={loading} scroll={{ x: 900 }} locale={{ emptyText: <GraphicEmpty title="还没有分享" description="在“我的云盘”或搜索结果中选择文件创建分享。" /> }} />
      {page.has_more && <Button loading={loading} onClick={() => load(page.next_cursor)}>加载更多</Button>}
    </Space>
    <ShareModal open={Boolean(editing)} share={editing} onCancel={() => setEditing(null)} onSaved={() => load()} />
  </ProductPage>;
}
