import React, { useEffect, useState } from 'react';
import { App, Button, Input, Modal, Tag, Tooltip } from 'antd';
import {
  DeleteOutlined,
  DownloadOutlined,
  EditOutlined,
  FolderOpenOutlined,
  LinkOutlined,
} from '@ant-design/icons';
import styled from '@emotion/styled';
import { deleteFile, getBreadcrumb, getDownloadURL, getRoot, renameFile } from '../services/drive';
import { createShare } from '../services/share';
import { FileTypeBadge, fileTypeLabel, StatusTag } from './design/ProductUI';
import MoveFileModal from './MoveFileModal';

const FileIntro = styled.div`
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  gap: 14px;
  align-items: center;
  padding-bottom: 18px;
  border-bottom: 1px solid var(--divider);
`;
const FileName = styled.h3`margin: 0; overflow-wrap: anywhere; font-size: 18px; line-height: 1.35;`;
const FilePath = styled.p`margin: 5px 0 0; color: var(--muted); font-size: 13px; line-height: 1.5; overflow-wrap: anywhere;`;
const MetaRow = styled.div`display: flex; flex-wrap: wrap; gap: 8px 18px; margin: 16px 0 4px; color: var(--muted); font-size: 13px;`;
const CitationList = styled.div`display: grid; margin-top: 16px; border-top: 1px solid var(--divider);`;
const Citation = styled.div`padding: 14px 0; border-bottom: 1px solid var(--divider); color: var(--muted); font-size: 13px; line-height: 1.65;`;
const CitationLabel = styled.div`margin-top: 8px;`;
const EmptyCitation = styled.p`margin: 18px 0 2px; color: var(--text-tertiary); font-size: 13px;`;
const ModalActions = styled.div`
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  justify-content: flex-end;

  @media (max-width: 560px) {
    justify-content: stretch;
    .ant-btn { flex: 1 1 calc(50% - 8px); }
  }
`;

const formatSize = bytes => {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
};

const matchLabels = {
  hybrid: '混合检索',
  name: '文件名',
  fulltext: '全文',
  semantic: '语义',
};

const SearchResultModal = ({ hit, open, onClose, onChanged }) => {
  const { message, modal } = App.useApp();
  const file = hit?.file;
  const [path, setPath] = useState('');
  const [loading, setLoading] = useState('');
  const [renameOpen, setRenameOpen] = useState(false);
  const [renameName, setRenameName] = useState('');
  const [moveOpen, setMoveOpen] = useState(false);
  const [root, setRoot] = useState(null);

  useEffect(() => {
    if (!open || !file?.folder_id) return undefined;
    let active = true;
    setPath('');
    getBreadcrumb(file.folder_id)
      .then(response => {
        if (!active) return;
        const value = (response.items || []).map(item => item.name === '/' ? '根目录' : item.name).join(' / ');
        setPath(value || '所在目录不可用');
      })
      .catch(() => active && setPath('所在目录不可用'));
    return () => { active = false; };
  }, [file?.folder_id, open]);

  const finishChange = async successMessage => {
    onClose();
    await onChanged?.();
    message.success(successMessage);
  };

  const download = async () => {
    if (!file || loading) return;
    setLoading('download');
    try {
      const value = await getDownloadURL(file.id);
      const link = document.createElement('a');
      link.href = value.url;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      link.click();
    } catch (error) {
      message.error(error.message || '下载地址获取失败');
    } finally {
      setLoading('');
    }
  };

  const copyShareKey = async value => {
    try {
      await navigator.clipboard.writeText(value);
      message.success('分享 Key 已复制');
    } catch {
      message.error('复制失败，请手动选择分享 Key');
    }
  };

  const share = async () => {
    if (!file || loading) return;
    setLoading('share');
    try {
      const result = await createShare(file.id);
      modal.info({
        title: '分享 Key',
        content: <Input readOnly value={result.share_key} addonAfter={<Tooltip title="复制"><Button aria-label="复制分享 Key" type="text" icon={<LinkOutlined />} onClick={() => copyShareKey(result.share_key)} /></Tooltip>} />,
        okText: '关闭',
      });
    } catch (error) {
      message.error(error.message || '分享创建失败');
    } finally {
      setLoading('');
    }
  };

  const remove = () => file && modal.confirm({
    title: `删除 ${file.name}?`,
    content: '删除后将从当前网盘中移除。',
    okText: '删除',
    cancelText: '取消',
    okButtonProps: { danger: true },
    onOk: async () => {
      try {
        await deleteFile(file.id, file.version);
        await finishChange('文件已删除');
      } catch (error) {
        message.error(error.message || '文件删除失败');
        throw error;
      }
    },
  });

  const openRename = () => {
    setRenameName(file?.name || '');
    setRenameOpen(true);
  };

  const rename = async () => {
    const name = renameName.trim();
    if (!file || !name || loading) return;
    if (name === file.name) {
      setRenameOpen(false);
      return;
    }
    setLoading('rename');
    try {
      await renameFile(file.id, file.version, name);
      setRenameOpen(false);
      await finishChange('文件已重命名');
    } catch (error) {
      message.error(error.message || '文件重命名失败');
    } finally {
      setLoading('');
    }
  };

  const openMove = async () => {
    if (root) {
      setMoveOpen(true);
      return;
    }
    setLoading('move');
    try {
      setRoot(await getRoot());
      setMoveOpen(true);
    } catch (error) {
      message.error(error.message || '根目录加载失败');
    } finally {
      setLoading('');
    }
  };

  const moved = async () => {
    setMoveOpen(false);
    await finishChange('文件已移动');
  };

  return <>
    <Modal
      open={Boolean(open && file && !renameOpen && !moveOpen)}
      title="文件详情"
      width={760}
      onCancel={onClose}
      destroyOnClose
      footer={<ModalActions>
        <Button aria-label="下载" icon={<DownloadOutlined />} loading={loading === 'download'} onClick={download}>下载</Button>
        <Button aria-label="分享" icon={<LinkOutlined />} loading={loading === 'share'} onClick={share}>分享</Button>
        <Button aria-label="移动到" icon={<FolderOpenOutlined />} loading={loading === 'move'} onClick={openMove}>移动到</Button>
        <Button aria-label="重命名" icon={<EditOutlined />} onClick={openRename}>重命名</Button>
        <Button aria-label="删除" danger icon={<DeleteOutlined />} onClick={remove}>删除</Button>
      </ModalActions>}
    >
      {file && <>
        <FileIntro>
          <FileTypeBadge type={file.mime_type} />
          <div><FileName>{file.name}</FileName><FilePath>{path || '正在加载所在目录…'}</FilePath></div>
        </FileIntro>
        <MetaRow>
          <span>{fileTypeLabel(file.mime_type)}</span>
          <span>{formatSize(file.size_bytes || 0)}</span>
          <StatusTag tone="blue">{matchLabels[hit.match_type] || hit.match_type}</StatusTag>
        </MetaRow>
        {hit.citations?.length > 0
          ? <CitationList>{hit.citations.map(citation => <Citation key={citation.id}>“{citation.excerpt}”<CitationLabel><Tag color="blue">{citation.page_number ? `第 ${citation.page_number} 页` : citation.section || '正文'}</Tag></CitationLabel></Citation>)}</CitationList>
          : <EmptyCitation>该结果由文件名匹配，没有内容摘录。</EmptyCitation>}
      </>}
    </Modal>
    <Modal
      open={Boolean(renameOpen && file)}
      title="重命名文件"
      okText="保存"
      cancelText="取消"
      confirmLoading={loading === 'rename'}
      onOk={rename}
      onCancel={() => setRenameOpen(false)}
      destroyOnClose
    >
      <Input autoFocus aria-label="新文件名" value={renameName} onChange={event => setRenameName(event.target.value)} onPressEnter={rename} />
    </Modal>
    <MoveFileModal
      open={Boolean(moveOpen && file && root)}
      file={file}
      root={root}
      currentFolderId={file?.folder_id}
      onCancel={() => setMoveOpen(false)}
      onMoved={moved}
    />
  </>;
};

export default SearchResultModal;
