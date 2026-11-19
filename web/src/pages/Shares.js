import React, { useCallback, useEffect, useRef, useState } from 'react';
import { App, Button, Grid, Space, Table, Tabs, Tooltip } from 'antd';
import { CalendarOutlined, StopOutlined } from '@ant-design/icons';
import { listShares, revokeShare } from '../services/share';
import { FileTypeBadge, fileTypeLabel, PageHeader, ProductPage, StatusTag } from '../components/design/ProductUI';
import ShareModal, { formatShareExpiry } from '../components/ShareModal';

const PAGE_SIZE = 10;
const tabs = [
  { key: 'active', label: '分享有效' }, { key: 'expired', label: '分享过期' }, { key: 'revoked', label: '分享撤销' }, { key: 'source_deleted', label: '源文件已删除' },
];
export const shareStatus = (share, now = Date.now()) => {
  if (share.source_deleted_at || share.status === 'source_deleted') return 'source_deleted';
  if (share.revoked_at || share.status === 'revoked') return 'revoked';
  if (share.status === 'expired' || (share.expires_at && new Date(share.expires_at).valueOf() <= now)) return 'expired';
  return 'active';
};

export default function Shares() {
  const { message } = App.useApp();
  const screens = Grid.useBreakpoint();
  const compact = !screens.md;
  // Match Drive's type column position on desktop, keeping room for the date.
  const expiryWidth = screens.lg ? 332 : 210;
  const [items, setItems] = useState([]);
  const [page, setPage] = useState({});
  const [query, setQuery] = useState({ status: 'active', cursors: [''], index: 0 });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [editing, setEditing] = useState(null);
  const [revoking, setRevoking] = useState([]);
  const [now, setNow] = useState(Date.now());
  const request = useRef(null);
  const pendingRevokes = useRef(new Set());
  const load = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController(); request.current = controller;
    setLoading(true); setError(''); message.destroy('shares-load-error');
    try {
      const result = await listShares({ status: query.status, cursor: query.cursors[query.index], limit: PAGE_SIZE, signal: controller.signal });
      if (controller.signal.aborted) return;
      setItems(result.items); setPage(result.page || {}); setNow(Date.now());
    } catch (err) {
      if (!controller.signal.aborted && err.name !== 'AbortError') {
        const text = err.message || '分享列表加载失败';
        setError(text);
        message.error({ key: 'shares-load-error', content: text });
      }
    }
    finally { if (!controller.signal.aborted) setLoading(false); }
  }, [message, query]);
  useEffect(() => { load(); return () => request.current?.abort(); }, [load]);
  useEffect(() => () => message.destroy('shares-load-error'), [message]);
  const refresh = useCallback(() => {
    setLoading(true); setItems([]); setPage({});
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
    request.current?.abort(); setLoading(true); setItems([]); setPage({}); setError(''); setEditing(null);
    setQuery({ status, cursors: [''], index: 0 });
  };
  const turnPage = direction => {
    setLoading(true); setItems([]); setPage({});
    setQuery(current => {
      const cursors = direction > 0 ? [...current.cursors.slice(0, current.index + 1), page.next_cursor] : current.cursors;
      return { ...current, cursors, index: current.index + direction };
    });
  };
  const revoke = async share => {
    if (pendingRevokes.current.has(share.id)) return;
    if (shareStatus(share) !== 'active') { refresh(); return; }
    pendingRevokes.current.add(share.id); setRevoking([...pendingRevokes.current]);
    try { await revokeShare(share.id); message.success('分享已取消'); refresh(); }
    catch (err) { message.error(err.message || '取消分享失败'); refresh(); }
    finally { pendingRevokes.current.delete(share.id); setRevoking([...pendingRevokes.current]); }
  };
  const renderActions = share => shareStatus(share, now) === 'active' && <div className="share-actions" aria-busy={revoking.includes(share.id)}>
    <Tooltip title="修改有效期"><Button shape="circle" icon={<CalendarOutlined />} aria-label="修改有效期" disabled={revoking.includes(share.id)} onClick={() => setEditing(share)} /></Tooltip>
    <Tooltip title="取消分享"><Button shape="circle" icon={<StopOutlined />} aria-label="取消分享" danger loading={revoking.includes(share.id)} onClick={() => revoke(share)} /></Tooltip>
  </div>;
  const columns = [
    { title: <span className="share-name-heading">名称</span>, dataIndex: 'file_name', render: (name, share) => <div className="share-file">
      <div className="share-name"><FileTypeBadge type={share.mime_type} /><div className="share-copy">
        <strong title={name || '未知文件'}>{name || '未知文件'}</strong>
        {compact && query.status === 'active' && <span className="share-mobile-expiry" title={formatShareExpiry(share.expires_at)}>{formatShareExpiry(share.expires_at)}</span>}
      </div></div>
      {compact && query.status === 'active' && renderActions(share)}
    </div> },
    ...(!compact ? [
      { title: <span className="share-type-heading">类型</span>, dataIndex: 'mime_type', width: 110, render: type => <StatusTag>{fileTypeLabel(type)}</StatusTag> },
      ...(query.status === 'active' ? [
        { title: '有效期', dataIndex: 'expires_at', width: expiryWidth, render: formatShareExpiry },
        { title: '', key: 'actions', width: 108, align: 'right', render: (_, share) => renderActions(share) },
      ] : [{ title: '', key: 'spacing', width: expiryWidth + 108, render: () => null }]),
    ] : []),
  ];
  return <ProductPage className="shares-page">
    <PageHeader title="我的分享" />
    <Tabs activeKey={query.status} items={tabs} onChange={changeTab} tabBarGutter={24} />
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Table className="editorial-ledger" aria-busy={loading} rowKey="id" columns={columns} dataSource={items} pagination={false} loading={loading} tableLayout="fixed" locale={{ emptyText: loading || error ? null : `暂无${query.status === 'source_deleted' ? '源文件已删除的' : tabs.find(tab => tab.key === query.status).label.slice(2)}分享` }} />
      <Space aria-label="分享分页" wrap>
        <Button disabled={loading || query.index === 0} onClick={() => turnPage(-1)}>上一页</Button>
        <span>第 {query.index + 1} 页 · 每页 {PAGE_SIZE} 条</span>
        <Button disabled={loading || !page.has_more || Boolean(error)} onClick={() => turnPage(1)}>下一页</Button>
      </Space>
    </Space>
    <ShareModal open={Boolean(editing)} share={editing} onCancel={() => setEditing(null)} onSaved={refresh} />
  </ProductPage>;
}
