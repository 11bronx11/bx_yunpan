import React, { useRef, useState } from 'react';
import { Alert, App, Button, DatePicker, Input, Modal, Select, Space } from 'antd';
import { CopyOutlined } from '@ant-design/icons';
import { createShare, updateShare } from '../services/share';

export const formatShareExpiry = value => value ? new Date(value).toLocaleString() : '长期有效';

// Mount a new form for each target so dates and one-time keys cannot leak
// between files. Both drive and search use this same creation flow.
const ShareForm = ({ file, share, onCancel, onSaved }) => {
  const { message } = App.useApp();
  const [period, setPeriod] = useState('7');
  const [date, setDate] = useState(null);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState(null);
  const pending = useRef(false);
  const save = async () => {
    if (pending.current) return;
    let expiresAt = null;
    if (period === 'custom') {
      if (!date || date.valueOf() <= Date.now()) { message.warning('请选择未来的过期时间'); return; }
      expiresAt = date.toISOString();
    } else if (period !== 'forever') {
      expiresAt = new Date(Date.now() + Number(period) * 86400000).toISOString();
    }
    pending.current = true; setBusy(true);
    try {
      if (share) {
        await updateShare(share.id, share.version, expiresAt);
        message.success('分享有效期已更新');
        onSaved?.(); onCancel();
      } else {
        setResult(await createShare(file.id, expiresAt));
        onSaved?.();
      }
    } catch (error) {
      message.error(error.message || '分享操作失败');
      if (error.code === 'share.version_conflict') { onSaved?.(); onCancel(); }
    } finally { pending.current = false; setBusy(false); }
  };
  const copy = async () => {
    try { await navigator.clipboard.writeText(result.share_key); message.success('分享 Key 已复制'); }
    catch { message.error('复制失败，请手动选择分享 Key'); }
  };
  return <Modal open title={result ? '分享已创建' : share ? '修改分享有效期' : '创建分享'} onCancel={onCancel}
    maskClosable={!busy} closable={!busy} keyboard={!busy}
    footer={result ? <Button onClick={onCancel}>关闭</Button> : <Space><Button disabled={busy} onClick={onCancel}>取消</Button><Button type="primary" loading={busy} onClick={save}>{share ? '保存' : '创建分享'}</Button></Space>}>
    <Space direction="vertical" size={16} style={{ width: '100%', paddingTop: 8 }}>
      <strong style={{ overflowWrap: 'anywhere' }}>{file?.name || share?.file_name}</strong>
      {result ? <>
        <Input aria-label="分享 Key" readOnly value={result.share_key} addonAfter={<Button aria-label="复制分享 Key" type="text" icon={<CopyOutlined />} onClick={copy} />} />
        <div>有效期：{formatShareExpiry(result.share?.expires_at)}</div>
        <Alert type="info" showIcon message="请保存分享 Key，关闭后无法再次查看。可在“我的分享”中修改有效期或取消分享。" />
      </> : <>
        {share && <div>当前有效期：{formatShareExpiry(share.expires_at)}</div>}
        <label htmlFor="share-period">有效期</label>
        <Select id="share-period" aria-label="有效期" value={period} onChange={setPeriod} style={{ width: '100%' }} options={[
          { value: '1', label: '1 天' }, { value: '7', label: '7 天' }, { value: '30', label: '30 天' },
          { value: 'forever', label: '长期有效' }, { value: 'custom', label: '自定义时间' },
        ]} />
        {period === 'custom' && <DatePicker aria-label="过期时间" showTime value={date} onChange={setDate} style={{ width: '100%' }} />}
        <Alert type="info" showIcon message={share ? '新有效期从本次保存起计算。已撤销的分享不能恢复。' : '持有 Key 的用户可导入文件。取消或过期会阻止后续访问，已经导入的文件不受影响。'} />
      </>}
    </Space>
  </Modal>;
};

const ShareModal = props => props.open && (props.file || props.share) ? <ShareForm key={props.share?.id || props.file?.id} {...props} /> : null;
export default ShareModal;
