import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { App, Button, Dropdown, Grid, Input, Modal, Space, Table, Tooltip, Upload } from 'antd';
import { DeleteOutlined, DownloadOutlined, EditOutlined, FolderAddOutlined, FolderOpenOutlined, FolderOutlined, ImportOutlined, LeftOutlined, LinkOutlined, MoreOutlined, RightOutlined, RobotOutlined, UploadOutlined } from '@ant-design/icons';
import { useNavigate } from 'react-router-dom';
import { useAuth } from '../contexts/AuthContext';
import { useTransfers } from '../contexts/TransferContext';
import { createFolder, deleteFile, deleteFolder, getBreadcrumb, getChildren, getDownloadURL, getPreview, getRoot, renameFile } from '../services/drive';
import { createShare, importShare, resolveShare } from '../services/share';
import { FileTypeBadge, fileTypeLabel, GraphicEmpty, PageHeader, ProductPage, StatusTag } from '../components/design/ProductUI';
import MoveFileModal from '../components/MoveFileModal';
import AIIndexModal from '../components/AIIndexModal';
import FileSummaryModal from '../components/FileSummaryModal';
import styled from '@emotion/styled';

const Workspace = styled.div`display: grid; min-width: 0; gap: 16px;`;
const ActionGroup = styled.div`display: flex; flex-wrap: wrap; gap: 8px; justify-content: flex-end;`;
const RowActions = styled.div`
  display: flex;
  gap: 4px;
  align-items: center;
  justify-content: flex-end;
  opacity: 0;
  visibility: hidden;
  pointer-events: none;
  transform: translateX(4px);
  transition: opacity var(--transition-fast), transform var(--transition-fast), visibility var(--transition-fast);

  .ant-table-row:hover &,
  .ant-table-row:focus &,
  .ant-table-row:focus-within & {
    opacity: 1;
    visibility: visible;
    pointer-events: auto;
    transform: translateX(0);
  }
`;
const FolderActions = styled(RowActions)`padding-right: var(--space-sm);`;
const DirectoryRow = styled.nav`display: flex; min-width: 0; gap: 12px; align-items: center; padding: 2px 0 4px;`;
const FolderRow = styled.ol`display: flex; min-width: 0; gap: 6px; align-items: center; margin: 0; padding: 0; overflow-x: auto; list-style: none; scrollbar-width: thin;`;
const FolderCrumb = styled.button`display: inline-flex; gap: 7px; align-items: center; min-height: 38px; padding: 0 11px; border: 1px solid var(--divider); border-radius: var(--radius-pill); color: var(--ink); background: var(--white); cursor: pointer; white-space: nowrap; &:hover { border-color: var(--ink); background: var(--surface-hover); } &[aria-current="page"] { border-color: var(--ink); background: var(--surface-soft); font-weight: 600; }`;
const CrumbSeparator = styled(RightOutlined)`flex: 0 0 auto; color: var(--text-tertiary); font-size: 10px;`;
const Thumb = styled.img`display: block; width: 42px; height: 42px; border: 1px solid var(--divider); border-radius: 10px; object-fit: cover; background: var(--paper);`;
const NameCell = styled(Space)`min-width: 0; max-width: 100%; .ant-space-item:last-child { min-width: 0; }`;
const FileName = styled.strong`display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;`;
const PreviewShell = styled.div`
  display: grid;
  min-height: 280px;
  max-height: 74vh;
  place-items: center;
  overflow: auto;
  border: 1px solid var(--divider);
  border-radius: var(--radius-control);
  background: var(--surface-soft);

  img { display: block; max-width: 100%; max-height: 70vh; object-fit: contain; }
`;
const ShareFields = styled.div`display: grid; gap: 16px; padding-top: 8px;`;
const SharePreview = styled.div`display: grid; grid-template-columns: auto minmax(0, 1fr) auto; gap: 12px; align-items: center; padding: 14px; border: 1px solid var(--divider); border-radius: var(--radius-control); background: var(--surface-soft); @media (max-width: 520px) { grid-template-columns: auto minmax(0, 1fr); > :last-child { grid-column: 1 / -1; justify-self: start; } }`;
const ShareMeta = styled.div`min-width: 0; color: var(--muted); font-size: 12px; strong { display: block; margin-bottom: 3px; overflow: hidden; color: var(--ink); font-size: 14px; text-overflow: ellipsis; white-space: nowrap; }`;

const { useBreakpoint } = Grid;

const formatSize = bytes => {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
};

const aiStatusMeta = {
  pending: { label: '等待处理', tone: 'blue' },
  processing: { label: '处理中', tone: 'blue' },
  indexed: { label: '已索引', tone: 'mint' },
  failed: { label: '失败', tone: 'coral' },
  unsupported: { label: '未索引', tone: 'coral' },
};

const summaryMimeTypes = new Set([
  'text/plain',
  'text/markdown',
  'text/x-markdown',
  'application/json',
  'text/csv',
  'application/pdf',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
]);

const supportsAISummary = file => summaryMimeTypes.has((file?.mime_type || '').toLowerCase().split(';')[0].trim());

const Drive = () => {
  const { message, modal } = App.useApp();
  const { logout } = useAuth();
  const { enqueueUploads } = useTransfers();
  const navigate = useNavigate();
  const screens = useBreakpoint();
  const compactActions = !screens.lg;
  const [root, setRoot] = useState(null);
  const [folder, setFolder] = useState(null);
  const [items, setItems] = useState([]);
  const [crumbs, setCrumbs] = useState([]);
  const [createOpen, setCreateOpen] = useState(false);
  const [folderName, setFolderName] = useState('');
  const [creatingFolder, setCreatingFolder] = useState(false);
  const [renameTarget, setRenameTarget] = useState(null);
  const [renameName, setRenameName] = useState('');
  const [renaming, setRenaming] = useState(false);
  const [moveTarget, setMoveTarget] = useState(null);
  const [imagePreview, setImagePreview] = useState(null);
  const [shareOpen, setShareOpen] = useState(false);
  const [shareKey, setShareKey] = useState('');
  const [shareResult, setShareResult] = useState(null);
  const [shareResolving, setShareResolving] = useState(false);
  const [shareImporting, setShareImporting] = useState(false);
  const [previews, setPreviews] = useState({});
  const [aiTarget, setAITarget] = useState(null);
  const [summaryTarget, setSummaryTarget] = useState(null);

  const loadFolder = useCallback(async folderId => {
    try {
      const [children, breadcrumb] = await Promise.all([getChildren(folderId), getBreadcrumb(folderId)]);
      setFolder(folderId); setItems(children.items || []); setCrumbs(breadcrumb.items || []);
      const imageItems = (children.items || []).filter(item => item.type === 'file' && item.mime_type?.startsWith('image/'));
      const values = await Promise.all(imageItems.map(async item => [item.id, await getPreview(item.id).catch(() => null)]));
      setPreviews(current => ({ ...current, ...Object.fromEntries(values.filter(([, value]) => value?.url)) }));
    } catch (error) {
      if (error.status === 401) logout();
      else message.error(error.message || '目录加载失败');
    }
  }, [logout, message]);

  useEffect(() => { getRoot().then(value => { setRoot(value); return loadFolder(value.id); }).catch(error => message.error(error.message || '网盘加载失败')); }, [loadFolder, message]);

  const folders = useMemo(() => items.filter(item => item.type === 'folder'), [items]);
  const files = useMemo(() => items.filter(item => item.type === 'file'), [items]);
  const parentFolder = crumbs.length > 1 ? crumbs[crumbs.length - 2] : null;

  const handleCreate = async () => {
    if (!folderName.trim()) return;
    setCreatingFolder(true);
    try {
      await createFolder(folder, folderName.trim());
      setFolderName(''); setCreateOpen(false); await loadFolder(folder); message.success('目录已创建');
    } catch (error) {
      message.error(error.message || '目录创建失败');
    } finally {
      setCreatingFolder(false);
    }
  };

  const handleUpload = (file, fileList) => {
    if (file.uid !== fileList[0]?.uid) return false;
    const destination = crumbs.map(item => item.name === '/' ? '根目录' : item.name).join(' / ');
    const result = enqueueUploads(fileList, folder, destination);
    if (result.rejected > 0) message.warning(`单次最多上传 10 个文件，已忽略 ${result.rejected} 个`);
    else message.success(`已加入 ${result.accepted} 个上传任务`);
    navigate('/transfers');
    return false;
  };

  const handleDownload = async file => {
    try {
      const value = await getDownloadURL(file.id);
      const link = document.createElement('a');
      link.href = value.url; link.target = '_blank'; link.rel = 'noopener noreferrer'; link.click();
    } catch (error) {
      message.error(error.message || '下载地址获取失败');
    }
  };
  const handleDelete = file => modal.confirm({
    title: `删除 ${file.name}?`,
    content: '删除后将从当前网盘中移除。',
    okText: '删除',
    cancelText: '取消',
    okButtonProps: { danger: true },
    onOk: async () => {
      try { await deleteFile(file.id, file.version); await loadFolder(folder); message.success('文件已删除'); }
      catch (error) { message.error(error.message || '文件删除失败'); }
    },
  });
  const handleDeleteFolder = item => modal.confirm({
    title: `删除目录 ${item.name}?`,
    content: '只有完全空的目录才能删除。',
    okText: '删除',
    cancelText: '取消',
    okButtonProps: { danger: true },
    onOk: async () => {
      try { await deleteFolder(item.id, item.version); await loadFolder(folder); message.success('目录已删除'); }
      catch (error) { message.error(error.message || '目录删除失败'); }
    },
  });
  const openRename = file => { setRenameTarget(file); setRenameName(file.name); };
  const handleRename = async () => {
    const name = renameName.trim();
    if (!renameTarget || !name) return;
    if (name === renameTarget.name) { setRenameTarget(null); return; }
    setRenaming(true);
    try {
      await renameFile(renameTarget.id, renameTarget.version, name);
      setRenameTarget(null); await loadFolder(folder); message.success('文件已重命名');
    } catch (error) {
      message.error(error.message || '文件重命名失败');
    } finally {
      setRenaming(false);
    }
  };
  const handleMoved = async destination => {
    setMoveTarget(null);
    await loadFolder(folder);
    message.success(`已移动到 ${destination}`);
  };
  const openImagePreview = async file => {
    if (!file.mime_type?.startsWith('image/')) return;
    try {
      const preview = previews[file.id] || await getPreview(file.id);
      if (!previews[file.id]) setPreviews(current => ({ ...current, [file.id]: preview }));
      setImagePreview({ name: file.name, url: preview.url });
    } catch (error) {
      message.error(error.message || '图片预览加载失败');
    }
  };
  const copyShareKey = async shareKeyValue => {
    try { await navigator.clipboard.writeText(shareKeyValue); message.success('分享 Key 已复制'); }
    catch { message.error('复制失败，请手动选择分享 Key'); }
  };
  const handleShare = async file => {
    try {
      const result = await createShare(file.id);
      Modal.info({ title: '分享 Key', content: <Input readOnly value={result.share_key} addonAfter={<Tooltip title="复制"><Button aria-label="复制分享 Key" type="text" icon={<LinkOutlined />} onClick={() => copyShareKey(result.share_key)} /></Tooltip>} />, okText: '关闭' });
    } catch (error) {
      message.error(error.message || '分享创建失败');
    }
  };
  const openShareImport = () => { setShareKey(''); setShareResult(null); setShareOpen(true); };
  const handleResolveShare = async () => {
    if (!shareKey.trim()) { message.warning('请输入分享 Key'); return; }
    setShareResolving(true);
    try { setShareResult(await resolveShare(shareKey.trim())); }
    catch (error) { setShareResult(null); message.error(error.message || '分享 Key 无效'); }
    finally { setShareResolving(false); }
  };
  const handleImportShare = async () => {
    if (!shareResult) { await handleResolveShare(); return; }
    setShareImporting(true);
    try {
      await importShare(shareResult.share.id, folder, shareResult.share_access_token);
      message.success('分享已导入当前目录');
      setShareOpen(false); setShareKey(''); setShareResult(null); await loadFolder(folder);
    } catch (error) { message.error(error.message || '导入失败'); }
    finally { setShareImporting(false); }
  };

  const handleAIStatusChange = useCallback((fileId, status) => {
    setItems(current => current.map(item => item.id === fileId ? { ...item, ai_status: status || undefined } : item));
  }, []);

  const runFileAction = (file, key) => {
    if (key === 'download') handleDownload(file);
    if (key === 'delete') handleDelete(file);
    if (key === 'share') handleShare(file);
    if (key === 'move') setMoveTarget(file);
    if (key === 'rename') openRename(file);
    if (key === 'ai') setAITarget(file);
  };

  const compactFileMenu = file => ({
    items: [
      { key: 'download', icon: <DownloadOutlined />, label: '下载' },
      { key: 'delete', icon: <DeleteOutlined />, label: '删除', danger: true },
      { key: 'share', icon: <LinkOutlined />, label: '分享' },
      { key: 'ai', icon: <RobotOutlined />, label: 'AI 索引' },
      { key: 'move', icon: <FolderOpenOutlined />, label: '移动到' },
      { key: 'rename', icon: <EditOutlined />, label: '重命名' },
    ],
    onClick: ({ key, domEvent }) => { domEvent.stopPropagation(); runFileAction(file, key); },
  });

  const compactFolderMenu = item => ({
    items: [{ key: 'delete', icon: <DeleteOutlined />, label: '删除目录', danger: true }],
    onClick: ({ key, domEvent }) => { domEvent.stopPropagation(); if (key === 'delete') handleDeleteFolder(item); },
  });

  const moreFileMenu = file => ({
    items: [
      { key: 'ai', icon: <RobotOutlined />, label: 'AI 索引' },
      { key: 'move', icon: <FolderOpenOutlined />, label: '移动到' },
      { key: 'rename', icon: <EditOutlined />, label: '重命名' },
    ],
    onClick: ({ key }) => runFileAction(file, key),
  });

  const columns = [
    { title: '名称', key: 'name', render: (_, item) => <NameCell>{previews[item.id]?.url ? <Thumb src={previews[item.id].url} alt="" /> : <FileTypeBadge type={item.type === 'folder' ? 'folder' : item.mime_type} />}<FileName title={item.name}>{item.name}</FileName></NameCell> },
    { title: '类型', key: 'type', width: 110, responsive: ['md'], render: (_, item) => <StatusTag tone={item.type === 'folder' ? 'blue' : undefined}>{fileTypeLabel(item.type === 'folder' ? 'folder' : item.mime_type)}</StatusTag> },
    { title: '大小', key: 'size', width: 120, responsive: ['md'], render: (_, item) => item.type === 'folder' ? '-' : formatSize(item.size_bytes) },
    { title: '索引', key: 'ai', width: 110, responsive: ['lg'], render: (_, item) => item.type === 'folder' ? null : item.ai_status ? <StatusTag tone={aiStatusMeta[item.ai_status]?.tone}>{aiStatusMeta[item.ai_status]?.label || item.ai_status}</StatusTag> : <span style={{ color: 'var(--text-tertiary)' }}>未建立</span> },
    { title: '', key: 'action', width: compactActions ? 68 : 210, render: (_, item) => item.type === 'folder'
      ? compactActions
        ? <Dropdown menu={compactFolderMenu(item)} trigger={['click']}><Button aria-label={`管理目录 ${item.name}`} icon={<MoreOutlined />} onClick={event => event.stopPropagation()} /></Dropdown>
        : <FolderActions aria-label={`${item.name} 目录操作`} onClick={event => event.stopPropagation()}><Tooltip title="删除目录"><Button aria-label={`删除目录 ${item.name}`} danger icon={<DeleteOutlined />} onClick={() => handleDeleteFolder(item)} /></Tooltip></FolderActions>
      : compactActions
        ? <Dropdown menu={compactFileMenu(item)} trigger={['click']}><Button aria-label={`管理 ${item.name}`} icon={<MoreOutlined />} onClick={event => event.stopPropagation()} /></Dropdown>
        : <RowActions aria-label={`${item.name} 文件操作`} onClick={event => event.stopPropagation()}><Tooltip title="下载"><Button aria-label={`下载 ${item.name}`} icon={<DownloadOutlined />} onClick={() => handleDownload(item)} /></Tooltip><Tooltip title="删除"><Button aria-label={`删除 ${item.name}`} danger icon={<DeleteOutlined />} onClick={() => handleDelete(item)} /></Tooltip><Tooltip title="分享"><Button aria-label={`分享 ${item.name}`} icon={<LinkOutlined />} onClick={() => handleShare(item)} /></Tooltip><Dropdown menu={moreFileMenu(item)} trigger={['click']}><Tooltip title="更多"><Button aria-label={`更多操作 ${item.name}`} icon={<MoreOutlined />} /></Tooltip></Dropdown></RowActions> },
  ];

  const rowInteraction = item => {
    if (item.type === 'folder') return {
      tabIndex: 0,
      'aria-label': `进入目录 ${item.name}`,
      onClick: () => loadFolder(item.id),
      onKeyDown: event => {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        event.preventDefault();
        loadFolder(item.id);
      },
      style: { cursor: 'pointer' },
    };
    if (item.mime_type?.startsWith('image/')) return {
      tabIndex: 0,
      'aria-label': `预览图片 ${item.name}`,
      onClick: () => openImagePreview(item),
      onKeyDown: event => {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        event.preventDefault();
        openImagePreview(item);
      },
      style: { cursor: 'pointer' },
    };
    if (supportsAISummary(item)) return {
      tabIndex: 0,
      'aria-label': `查看 AI 摘要 ${item.name}`,
      onClick: () => setSummaryTarget(item),
      onKeyDown: event => {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        event.preventDefault();
        setSummaryTarget(item);
      },
      style: { cursor: 'pointer' },
    };
    return {};
  };

  if (!root || !folder) return <ProductPage><GraphicEmpty title="正在打开你的云盘" description="目录与对象索引加载中。" /></ProductPage>;
  return <ProductPage accent="var(--lime)"><Workspace>
    <PageHeader title="我的云盘" description="目录、秒传、Multipart 上传和对象级缩略图都在这里。" action={<ActionGroup><Button icon={<ImportOutlined />} onClick={openShareImport}>导入分享</Button><Button icon={<FolderAddOutlined />} onClick={() => setCreateOpen(true)}>新建目录</Button><Upload multiple showUploadList={false} beforeUpload={handleUpload}><Button type="primary" icon={<UploadOutlined />}>上传文件</Button></Upload></ActionGroup>} />
    <DirectoryRow aria-label="目录导航"><Tooltip title="返回上级目录"><Button aria-label="返回上级目录" icon={<LeftOutlined />} disabled={!parentFolder} onClick={() => parentFolder && loadFolder(parentFolder.id)} /></Tooltip><FolderRow>{crumbs.map((item, index) => <React.Fragment key={item.id}>{index > 0 && <CrumbSeparator aria-hidden="true" />}<li><FolderCrumb type="button" aria-current={item.id === folder ? 'page' : undefined} onClick={() => loadFolder(item.id)}><FolderOutlined />{item.name === '/' ? '根目录' : item.name}</FolderCrumb></li></React.Fragment>)}</FolderRow></DirectoryRow>
    {items.length === 0 ? <GraphicEmpty title="这个目录还是空的" description="新建目录或上传第一个文件。" /> : <Table className="editorial-ledger" rowKey="id" columns={columns} dataSource={[...folders, ...files]} pagination={false} tableLayout="fixed" onRow={rowInteraction} />}
    <Modal open={createOpen} title="新建目录" okText="创建" cancelText="取消" confirmLoading={creatingFolder} onOk={handleCreate} onCancel={() => setCreateOpen(false)}><Input autoFocus aria-label="目录名称" value={folderName} onChange={event => setFolderName(event.target.value)} onPressEnter={handleCreate} placeholder="目录名称" /></Modal>
    <Modal open={Boolean(renameTarget)} title="重命名文件" okText="保存" cancelText="取消" confirmLoading={renaming} onOk={handleRename} onCancel={() => setRenameTarget(null)} destroyOnClose><Input autoFocus aria-label="新文件名" value={renameName} onChange={event => setRenameName(event.target.value)} onPressEnter={handleRename} placeholder="新文件名" /></Modal>
    <MoveFileModal open={Boolean(moveTarget)} file={moveTarget} root={root} currentFolderId={folder} onCancel={() => setMoveTarget(null)} onMoved={handleMoved} />
    <AIIndexModal file={aiTarget} open={Boolean(aiTarget)} onClose={() => setAITarget(null)} onStatusChange={handleAIStatusChange} />
    <FileSummaryModal file={summaryTarget} open={Boolean(summaryTarget)} onClose={() => setSummaryTarget(null)} onManageIndex={file => { setSummaryTarget(null); setAITarget(file); }} />
    <Modal open={Boolean(imagePreview)} title={imagePreview?.name || '图片预览'} footer={null} width={920} centered destroyOnClose onCancel={() => setImagePreview(null)}><PreviewShell>{imagePreview && <img src={imagePreview.url} alt={imagePreview.name} />}</PreviewShell></Modal>
    <Modal open={shareOpen} title="导入分享" okText={shareResult ? '导入到当前目录' : '解析分享'} cancelText="取消" confirmLoading={shareResolving || shareImporting} onOk={handleImportShare} onCancel={() => setShareOpen(false)} destroyOnClose><ShareFields><Input autoFocus prefix={<LinkOutlined />} value={shareKey} onChange={event => { setShareKey(event.target.value); setShareResult(null); }} onPressEnter={handleResolveShare} placeholder="粘贴分享 Key" />{shareResult && <SharePreview><ImportOutlined /><ShareMeta><strong title={shareResult.share.file_name}>{shareResult.share.file_name}</strong><span>{shareResult.share.mime_type} · {formatSize(shareResult.share.size_bytes)}</span></ShareMeta><StatusTag tone="mint">可导入</StatusTag></SharePreview>}<div style={{ color: 'var(--muted)', fontSize: 12 }}>目标位置：{crumbs.map(item => item.name === '/' ? '根目录' : item.name).join(' / ')}</div></ShareFields></Modal>
  </Workspace></ProductPage>;
};

export default Drive;
