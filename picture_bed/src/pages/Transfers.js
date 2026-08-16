import React, { useMemo, useState } from 'react';
import { Button, Progress, Segmented, Tooltip, Upload } from 'antd';
import { CloseOutlined, DeleteOutlined, FileSearchOutlined, PauseOutlined, PlayCircleOutlined, RedoOutlined, UploadOutlined } from '@ant-design/icons';
import styled from '@emotion/styled';
import { GraphicEmpty, PageHeader, ProductPage, StatusTag } from '../components/design/ProductUI';
import { MAX_BATCH_FILES, MAX_CONCURRENT_UPLOADS, useTransfers } from '../contexts/TransferContext';

const Workspace = styled.div`display: grid; gap: 18px; min-width: 0;`;
const FilterRow = styled.div`display: flex; gap: 16px; align-items: center; justify-content: space-between; flex-wrap: wrap;`;
const QueueMeta = styled.span`color: var(--muted); font-size: 13px;`;
const TransferList = styled.div`overflow: hidden; border: 1px solid var(--divider-strong); border-radius: var(--radius-control); background: var(--white);`;
const TransferRow = styled.article`
  display: grid;
  grid-template-columns: minmax(220px, 1fr) minmax(220px, 360px) auto;
  gap: 24px;
  align-items: center;
  min-height: 92px;
  padding: 16px 18px;
  border-bottom: 1px solid var(--divider);
  &:last-child { border-bottom: 0; }
  @media (max-width: 860px) { grid-template-columns: minmax(0, 1fr) auto; gap: 12px 16px; }
  @media (max-width: 560px) { grid-template-columns: minmax(0, 1fr); padding: 15px; }
`;
const FileInfo = styled.div`display: grid; grid-template-columns: 42px minmax(0, 1fr); gap: 12px; align-items: center; min-width: 0;`;
const FileMark = styled.span`display: grid; place-items: center; width: 42px; height: 42px; border: 1px solid var(--divider-strong); border-radius: 10px; background: var(--lime-soft); font-size: 16px;`;
const FileCopy = styled.div`min-width: 0; strong { display: block; overflow: hidden; margin-bottom: 4px; text-overflow: ellipsis; white-space: nowrap; } span { display: block; overflow: hidden; color: var(--muted); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }`;
const ProgressCell = styled.div`
  display: grid;
  gap: 7px;
  min-width: 0;
  @media (max-width: 860px) { grid-column: 1 / -1; grid-row: 2; }
  @media (max-width: 560px) { grid-row: auto; }
`;
const ProgressMeta = styled.div`
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  gap: 8px;
  align-items: start;
  min-width: 0;
  color: var(--muted);
  font-size: 12px;
`;
const DetailText = styled.span`min-width: 0; justify-self: end; line-height: 1.5; text-align: right; overflow-wrap: anywhere;`;
const FeedbackText = styled(DetailText)`color: ${({ $benign }) => $benign ? 'var(--text-secondary)' : '#a52c26'};`;
const RowActions = styled.div`
  display: flex;
  gap: 6px;
  align-items: center;
  justify-content: flex-end;
  @media (max-width: 560px) { justify-content: flex-start; }
`;

const unfinishedStatuses = new Set(['queued', 'checking', 'uploading', 'pausing', 'paused', 'verifying', 'verification_pending', 'interrupted']);
const finishedStatuses = new Set(['completed', 'skipped']);

const statusMeta = {
  queued: { label: '等待中', tone: 'blue' },
  checking: { label: '正在校验', tone: 'blue' },
  uploading: { label: '上传中', tone: 'mint' },
  pausing: { label: '正在暂停', tone: 'coral' },
  paused: { label: '已暂停', tone: 'coral' },
  verifying: { label: '服务器校验中', tone: 'blue' },
  verification_pending: { label: '等待校验结果', tone: 'coral' },
  interrupted: { label: '上传已中断', tone: 'coral' },
  completed: { label: '已完成', tone: 'lime' },
  skipped: { label: '已存在', tone: 'blue' },
  failed: { label: '上传失败', tone: 'pink' },
  canceled: { label: '已取消' },
};

const formatSize = bytes => {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
};

const Transfers = () => {
  const { uploads, activeCount, pauseUpload, resumeUpload, selectResumeFile, cancelUpload, removeUpload } = useTransfers();
  const [filter, setFilter] = useState('all');
  const completedCount = uploads.filter(task => task.status === 'completed').length;
  const skippedCount = uploads.filter(task => task.status === 'skipped').length;
  const finishedCount = completedCount + skippedCount;
  const interruptedCount = uploads.filter(task => task.status === 'interrupted').length;
  const filtered = useMemo(() => uploads.filter(task => {
    if (filter === 'active') return unfinishedStatuses.has(task.status);
    if (filter === 'completed') return finishedStatuses.has(task.status);
    return true;
  }), [filter, uploads]);

  let queueText = '当前没有上传任务';
  if (activeCount > 0) {
    queueText = interruptedCount === activeCount ? `${interruptedCount} 个上传等待恢复` : `${activeCount} 个任务尚未完成`;
  } else if (completedCount > 0 && skippedCount === 0) {
    queueText = `${completedCount} 个文件上传完成`;
  } else if (uploads.length > 0) {
    queueText = `${uploads.length} 个任务已结束`;
  }

  return <ProductPage accent="var(--blue)"><Workspace>
    <PageHeader
      title="传输"
      description={`单次最多 ${MAX_BATCH_FILES} 个文件，同时上传 ${MAX_CONCURRENT_UPLOADS} 个，支持分片与断点续传。`}
    />
    {uploads.length > 0 && <FilterRow>
      <Segmented value={filter} onChange={setFilter} options={[{ label: `全部 ${uploads.length}`, value: 'all' }, { label: `未完成 ${activeCount}`, value: 'active' }, { label: `已结束 ${finishedCount}`, value: 'completed' }]} />
      <QueueMeta aria-live="polite">{queueText}</QueueMeta>
    </FilterRow>}
    {filtered.length === 0
      ? <GraphicEmpty title={uploads.length === 0 ? '暂无上传任务' : '没有符合条件的上传任务'} description="从我的云盘选择文件后，任务会显示在这里。" />
      : <TransferList>{filtered.map(task => {
        const meta = task.status === 'skipped' && task.errorCode === 'upload.name_conflict'
          ? { label: '同名冲突', tone: 'blue' }
          : statusMeta[task.status] || statusMeta.queued;
        const canPause = ['queued', 'uploading'].includes(task.status);
        const canResume = ['paused', 'failed', 'verification_pending'].includes(task.status);
        const canCancel = ['queued', 'checking', 'uploading', 'pausing', 'paused', 'interrupted'].includes(task.status);
        const canRemove = ['failed', 'canceled'].includes(task.status);
        const benignFeedback = task.status === 'skipped';
        const progressStatus = task.status === 'failed' ? 'exception' : undefined;
        const progressColor = task.status === 'completed' ? 'var(--mint)' : task.status === 'skipped' ? 'var(--blue)' : 'var(--ink)';
        return <TransferRow key={task.id}>
          <FileInfo><FileMark><UploadOutlined /></FileMark><FileCopy><strong title={task.name}>{task.name}</strong><span title={task.destination}>{formatSize(task.size)} · {task.destination || '当前目录'}</span></FileCopy></FileInfo>
          <ProgressCell>
            <Progress percent={task.progress} status={progressStatus} showInfo={false} strokeColor={progressColor} />
            <ProgressMeta>
              <StatusTag tone={meta.tone}>{task.instant ? '秒传完成' : meta.label}</StatusTag>
              {task.error
                ? <FeedbackText role={benignFeedback ? undefined : 'alert'} $benign={benignFeedback} title={task.error}>{task.error}</FeedbackText>
                : <DetailText>{task.status === 'uploading' && task.speed > 0 ? `${formatSize(task.speed)}/s · ` : ''}{task.progress}%</DetailText>}
            </ProgressMeta>
          </ProgressCell>
          <RowActions>
            {task.status === 'interrupted' && <Upload maxCount={1} showUploadList={false} beforeUpload={file => selectResumeFile(task.id, file)}><Button icon={<FileSearchOutlined />}>选择原文件继续</Button></Upload>}
            {canPause && <Tooltip title="暂停"><Button aria-label={`暂停 ${task.name}`} icon={<PauseOutlined />} onClick={() => pauseUpload(task.id)} /></Tooltip>}
            {canResume && <Tooltip title={task.status === 'paused' ? '继续' : '重试'}><Button aria-label={`${task.status === 'paused' ? '继续' : '重试'} ${task.name}`} icon={task.status === 'paused' ? <PlayCircleOutlined /> : <RedoOutlined />} onClick={() => resumeUpload(task.id)} /></Tooltip>}
            {canCancel && <Tooltip title="取消"><Button aria-label={`取消 ${task.name}`} danger icon={<CloseOutlined />} onClick={() => cancelUpload(task.id)} /></Tooltip>}
            {canRemove && <Tooltip title="移除记录"><Button aria-label={`移除 ${task.name}`} icon={<DeleteOutlined />} onClick={() => removeUpload(task.id)} /></Tooltip>}
          </RowActions>
        </TransferRow>;
      })}</TransferList>}
  </Workspace></ProductPage>;
};

export default Transfers;
