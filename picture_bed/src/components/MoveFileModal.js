import React, { useCallback, useEffect, useState } from 'react';
import { Alert, App, Button, Modal, Spin, Tree } from 'antd';
import { FolderOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import { getBreadcrumb, getChildren, moveFile } from '../services/drive';

const Picker = styled.div`display: grid; gap: 14px; padding-top: 4px;`;
const Intro = styled.p`margin: 0; color: var(--muted); font-size: 13px; line-height: 1.6;`;
const TreeShell = styled.div`
  min-height: 220px;
  max-height: min(46vh, 360px);
  padding: 8px;
  overflow: auto;
  border: 1px solid var(--divider-strong);
  border-radius: var(--radius-control);
  background: var(--surface-soft);

  .ant-tree { background: transparent; }
  .ant-tree-treenode { box-sizing: border-box; width: 100%; min-width: 0; min-height: 44px; align-items: center; padding: 2px 0; }
  .ant-tree-node-content-wrapper { display: flex; flex: 1; min-width: 0; align-items: center; min-height: 40px; overflow: hidden; }
  .ant-tree-title { width: 100%; min-width: 0; }
`;
const Loading = styled.div`display: grid; min-height: 200px; place-items: center;`;
const FolderTitle = styled.span`display: flex; width: 100%; min-width: 0; gap: 8px; align-items: center; justify-content: space-between;`;
const FolderName = styled.span`flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;`;
const Current = styled.span`flex: 0 0 auto; color: var(--text-tertiary); font-size: 12px;`;
const Destination = styled.div`
  min-height: 42px;
  padding: 10px 12px;
  border: 1px solid var(--divider);
  border-radius: var(--radius-control);
  color: var(--muted);
  background: var(--white);
  font-size: 13px;
  line-height: 1.55;
  overflow-wrap: anywhere;
  strong { color: var(--ink); font-weight: 600; }
`;

const folderTitle = (name, current) => (
  <FolderTitle><FolderName title={name}>{name}</FolderName>{current && <Current>当前位置，不可选择</Current>}</FolderTitle>
);

const folderNode = (folder, currentFolderId) => ({
  key: folder.id,
  folderName: folder.name === '/' ? '根目录' : folder.name,
  title: folderTitle(folder.name === '/' ? '根目录' : folder.name, folder.id === currentFolderId),
  icon: <FolderOutlined />,
  disabled: folder.id === currentFolderId,
  isLeaf: false,
});

const replaceChildren = (nodes, key, children) => nodes.map(node => {
  if (node.key === key) return { ...node, children, isLeaf: children.length === 0 };
  if (!node.children) return node;
  return { ...node, children: replaceChildren(node.children, key, children) };
});

const directoryChildren = (response, currentFolderId) => (response.items || [])
  .filter(item => item.type === 'folder')
  .map(item => folderNode(item, currentFolderId));

const MoveFileModal = ({ open, file, root, currentFolderId, onCancel, onMoved }) => {
  const { message } = App.useApp();
  const [treeData, setTreeData] = useState([]);
  const [expandedKeys, setExpandedKeys] = useState([]);
  const [selectedFolderId, setSelectedFolderId] = useState(null);
  const [destination, setDestination] = useState('');
  const [loading, setLoading] = useState(false);
  const [moving, setMoving] = useState(false);
  const [browseError, setBrowseError] = useState('');

  const loadRoot = useCallback(async () => {
    if (!root) return;
    setLoading(true);
    setBrowseError('');
    try {
      const response = await getChildren(root.id);
      const node = folderNode(root, currentFolderId);
      node.children = directoryChildren(response, currentFolderId);
      node.isLeaf = node.children.length === 0;
      setTreeData([node]);
      setExpandedKeys([root.id]);
    } catch (error) {
      setBrowseError(error.message || '目录加载失败，请重试');
    } finally {
      setLoading(false);
    }
  }, [currentFolderId, root]);

  useEffect(() => {
    if (!open) return;
    setSelectedFolderId(null);
    setDestination('');
    setTreeData([]);
    loadRoot();
  }, [loadRoot, open]);

  const loadTreeNode = async node => {
    if (node.children || node.isLeaf) return;
    try {
      const response = await getChildren(node.key);
      setTreeData(current => replaceChildren(current, node.key, directoryChildren(response, currentFolderId)));
      setBrowseError('');
    } catch (error) {
      setBrowseError(error.message || '子目录加载失败，请重试展开');
      throw error;
    }
  };

  const selectDestination = async (keys, info) => {
    const folderId = keys[0];
    if (!folderId) return;
    setSelectedFolderId(folderId);
    setDestination(info.node.folderName);
    try {
      const response = await getBreadcrumb(folderId);
      const path = (response.items || []).map(item => item.name === '/' ? '根目录' : item.name).join(' / ');
      if (path) setDestination(path);
    } catch {
      // The selected folder name remains usable if the path preview fails.
    }
  };

  const submitMove = async () => {
    if (!file || !selectedFolderId || selectedFolderId === currentFolderId || moving) return;
    setMoving(true);
    try {
      await moveFile(file.id, file.version, selectedFolderId);
      await onMoved(destination);
    } catch (error) {
      message.error(error.message || '文件移动失败，请重试');
    } finally {
      setMoving(false);
    }
  };

  return (
    <Modal
      open={open}
      title={file ? `移动“${file.name}”` : '移动文件'}
      okText="移动"
      cancelText="取消"
      confirmLoading={moving}
      okButtonProps={{ disabled: !selectedFolderId || selectedFolderId === currentFolderId }}
      onOk={submitMove}
      onCancel={onCancel}
      destroyOnClose
      width={560}
    >
      <Picker>
        <Intro>选择目标目录。移动只改变文件所在位置，不会重新上传或修改文件内容。</Intro>
        {browseError && <Alert type="error" showIcon message={browseError} action={<Button size="small" onClick={loadRoot}>重试</Button>} />}
        <TreeShell>
          {loading
            ? <Loading><Spin /></Loading>
            : <Tree
                aria-label="目标目录"
                blockNode
                showIcon
                treeData={treeData}
                expandedKeys={expandedKeys}
                selectedKeys={selectedFolderId ? [selectedFolderId] : []}
                loadData={loadTreeNode}
                onExpand={setExpandedKeys}
                onSelect={selectDestination}
              />}
        </TreeShell>
        <Destination aria-live="polite">目标位置：<strong>{destination || '请选择目录'}</strong></Destination>
      </Picker>
    </Modal>
  );
};

export default MoveFileModal;
