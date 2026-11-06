import React, { useEffect, useRef, useState } from 'react';
import { Alert, App, Input, Modal, Space } from 'antd';
import { importShare, resolveShare } from '../services/share';
import { formatShareExpiry } from './ShareModal';

const ImportForm = ({ folderId, targetLabel, onCancel, onImported }) => {
  const { message } = App.useApp();
  const [key, setKey] = useState('');
  const [result, setResult] = useState(null);
  const [busy, setBusy] = useState('');
  const request = useRef(null);
  const importing = useRef(false);
  const idempotency = useRef(null);
  useEffect(() => () => request.current?.abort(), []);
  const change = event => {
    request.current?.abort(); setBusy(''); setKey(event.target.value); setResult(null); idempotency.current = null;
  };
  const submit = async () => {
    if (busy || importing.current) return;
    if (!result) {
      if (!/^[A-Za-z0-9_-]{22}$/.test(key.trim())) { message.warning('请输入完整的分享 Key'); return; }
      const controller = new AbortController(); request.current = controller; setBusy('resolve');
      try {
        const resolved = await resolveShare(key.trim(), { signal: controller.signal });
        if (!controller.signal.aborted) { setResult(resolved); idempotency.current = crypto.randomUUID(); }
      } catch (error) { if (!controller.signal.aborted) message.error(error.message || '解析分享失败'); }
      finally { if (!controller.signal.aborted) setBusy(''); }
      return;
    }
    importing.current = true; setBusy('import');
    try {
      await importShare(result.share.id, folderId, result.share_access_token, idempotency.current);
      message.success('分享已导入当前目录');
      onImported?.(); onCancel();
    } catch (error) {
      message.error(error.message || '导入分享失败');
      if (error.code === 'share.not_found') { setResult(null); idempotency.current = null; }
    } finally { importing.current = false; setBusy(''); }
  };
  return <Modal open title="导入分享" okText={result ? '导入到当前目录' : '解析分享'} cancelText="取消" onOk={submit}
    confirmLoading={Boolean(busy)} onCancel={onCancel} closable={busy !== 'import'} maskClosable={busy !== 'import'} keyboard={busy !== 'import'} cancelButtonProps={{ disabled: busy === 'import' }}>
    <Space direction="vertical" size={16} style={{ width: '100%', paddingTop: 8 }}>
      <Input autoFocus aria-label="分享 Key" disabled={busy === 'import'} value={key} onChange={change} onPressEnter={submit} placeholder="粘贴分享 Key" />
      {result && <Alert type="success" showIcon message={result.share.file_name} description={`有效期：${formatShareExpiry(result.share.expires_at)}`} />}
      <div>目标位置：{targetLabel}</div>
    </Space>
  </Modal>;
};
export default function ImportShareModal(props) { return props.open ? <ImportForm key={props.folderId} {...props} /> : null; }
