import React, { useRef, useState } from 'react';
import styled from '@emotion/styled';
import { Button, Modal } from 'antd';
import { EyeOutlined, FolderOutlined, InboxOutlined } from '@ant-design/icons';
import { useGSAP } from '@gsap/react';
import { gsap } from 'gsap';

gsap.registerPlugin(useGSAP);

const PageRoot = styled.main`
  position: relative;
  min-width: 0;
`;

const Header = styled.header`
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 24px;
  align-items: center;
  min-height: 104px;
  margin-bottom: 28px;
  padding: 12px 0 24px;
  border-bottom: 1px solid var(--divider);

  @media (max-width: 620px) {
    grid-template-columns: 1fr;
    gap: 14px;
    min-height: 0;
    margin-bottom: 22px;
    padding: 4px 0 20px;
  }
`;

const HeaderCopy = styled.div`
  min-width: 0;
`;

const TitleRow = styled.div`
  display: flex;
  gap: 10px;
  align-items: center;
`;

const AccentDot = styled.i`
  width: 10px;
  height: 10px;
  flex: 0 0 auto;
  border: 1px solid rgba(0, 0, 0, 0.65);
  border-radius: 46% 54% 43% 57% / 55% 42% 58% 45%;
  background: var(--page-accent, var(--lime));
  transform: rotate(-12deg);
`;

const Title = styled.h1`
  margin: 0;
  font-family: var(--body-font);
  font-size: clamp(30px, 3vw, 40px);
  font-weight: 700;
  line-height: 1.15;
  letter-spacing: 0;
`;

const Description = styled.p`
  margin: 10px 0 0 24px;
  color: var(--muted);
  font-size: 14px;
  line-height: 1.6;

  @media (max-width: 620px) { margin-left: 0; }
`;

const HeaderSide = styled.div`
  display: flex;
  gap: 12px;
  align-items: center;
  justify-content: flex-end;

  @media (max-width: 620px) { justify-content: space-between; }
`;

const Count = styled.span`
  color: var(--muted);
  font-size: 13px;
  white-space: nowrap;
`;

const SectionRow = styled.div`
  display: flex;
  gap: 16px;
  align-items: center;
  justify-content: space-between;
  margin: 0 0 16px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--divider);
`;

const SectionTitle = styled.h2`
  margin: 0;
  font-family: var(--body-font);
  font-size: clamp(21px, 2.5vw, 26px);
  font-weight: 600;
  line-height: 1.25;
  letter-spacing: 0;
`;

const SectionMeta = styled.div`
  color: var(--muted);
  font-size: 13px;
  font-weight: 400;
  color: var(--text-tertiary);
`;

const Tag = styled.span`
  display: inline-flex;
  gap: 6px;
  align-items: center;
  min-height: 25px;
  padding: 2px 9px;
  border: 1px solid var(--control-border);
  border-radius: var(--radius-pill);
  color: var(--ink);
  background: ${({ $tone }) => ({ lime: 'var(--lime-soft)', blue: 'var(--blue-soft)', pink: 'var(--pink-soft)', coral: 'var(--coral-soft)', mint: 'var(--mint-soft)', dark: 'var(--ink)' }[$tone] || 'var(--white)')};
  font-size: 12px;
  font-weight: 600;
  line-height: 1;
  ${({ $tone }) => $tone === 'dark' && 'color: var(--white);'}

  &::before {
    width: 6px;
    height: 6px;
    content: "";
    border-radius: 50%;
    background: currentColor;
  }
`;

const TypeBadge = styled.span`
  display: inline-grid;
  place-items: center;
  width: 42px;
  height: 36px;
  flex: 0 0 auto;
  border: 1px solid #d2d1cb;
  border-radius: 9px;
  background: ${({ $tone }) => $tone || 'var(--white)'};
  font-family: var(--utility-font);
  font-size: 11px;
  font-weight: 600;
`;

const PreviewFrame = styled.span`
  display: grid;
  place-items: center;
  width: ${({ $width }) => $width};
  height: ${({ $height }) => $height};
  flex: 0 0 auto;
  overflow: hidden;
  border: 1px solid #d2d1cb;
  border-radius: 9px;
  background: var(--surface-soft);

  img {
    width: 100%;
    height: 100%;
    object-fit: cover;
  }

  > ${TypeBadge} {
    width: 100%;
    height: 100%;
    border: 0;
    border-radius: 0;
  }
`;

const EmptyWrap = styled.div`
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  gap: 16px;
  align-items: center;
  min-height: 108px;
  padding: 20px 4px;
  border-top: 1px solid var(--divider);
  border-bottom: 1px solid var(--divider);
  background: transparent;

  @media (max-width: 560px) {
    grid-template-columns: auto 1fr;
    padding: 20px;
  }
`;

const EmptyAction = styled.div`
  @media (max-width: 560px) { grid-column: 1 / -1; }
`;

const EmptyMark = styled.div`
  display: grid;
  place-items: center;
  width: 44px;
  height: 44px;
  border: 1px solid var(--divider-strong);
  border-radius: 10px;
  background: var(--white);
  box-shadow: inset 0 -3px 0 var(--page-accent, var(--lime));
  font-size: 16px;
`;

const EmptyTitle = styled.h3`
  margin: 0 0 5px;
  font-family: var(--body-font);
  font-size: 16px;
  font-weight: 600;
`;

const EmptyCopy = styled.p`
  margin: 0;
  color: var(--muted);
  font-size: 13px;
  line-height: 1.55;
`;

const Toolbar = styled.div`
  display: flex;
  gap: 12px;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  margin-bottom: 24px;
  padding: 10px 12px;
  border: 1px solid #ecebe6;
  border-radius: var(--radius-control);
  background: var(--surface-soft);

  .toolbar-search { flex: 1; width: min(100%, 620px); }
  .toolbar-search .ant-input,
  .toolbar-search .ant-input-affix-wrapper {
    min-height: 52px;
    border-color: #d4d3cd;
    background: var(--white);
    font-size: 15px;
    box-shadow: 0 1px 2px rgba(0, 0, 0, 0.02);
  }
  .toolbar-search .ant-input-prefix {
    margin-right: 10px;
    color: var(--text-tertiary);
    font-size: 16px;
  }
  .toolbar-search .ant-input-affix-wrapper-focused {
    border-color: #292926;
    box-shadow: 0 0 0 3px rgba(198, 255, 0, 0.18);
  }
  .toolbar-filters { display: flex; gap: 4px; align-items: center; flex-wrap: wrap; }

  @media (max-width: 620px) {
    align-items: stretch;
    flex-direction: column;
    .toolbar-search { width: 100%; }
  }
`;

const DragLayer = styled.div`
  position: fixed;
  inset: 0;
  z-index: 70;
  display: grid;
  place-items: center;
  padding: 24px;
  background: rgba(247, 247, 242, 0.92);
`;

const DragBox = styled.div`
  display: grid;
  place-items: center;
  width: min(560px, 100%);
  min-height: 220px;
  padding: 32px;
  border: 1px solid var(--divider-strong);
  border-radius: var(--radius-panel);
  background: var(--page-accent, var(--lime));
  text-align: center;
  h2 { margin: 14px 0 7px; font-size: 28px; }
  p { margin: 0; font-size: 13px; }
  .anticon { font-size: 36px; }
`;

export const ProductPage = ({ accent = 'var(--lime)', children, className, onDragEnter, onDragOver, onDragLeave, onDrop }) => {
  const pageRef = useRef(null);

  useGSAP(() => {
    const mm = gsap.matchMedia();
    mm.add('(prefers-reduced-motion: no-preference)', () => {
      gsap.from('.page-reveal', {
        autoAlpha: 0,
        y: 8,
        duration: 0.24,
        stagger: 0.025,
        ease: 'power2.out',
        clearProps: 'transform,opacity,visibility',
      });
    });
    return () => mm.revert();
  }, { scope: pageRef });

  return (
    <PageRoot ref={pageRef} className={`product-page ${className || ''}`} style={{ '--page-accent': accent }} onDragEnter={onDragEnter} onDragOver={onDragOver} onDragLeave={onDragLeave} onDrop={onDrop}>
      {children}
    </PageRoot>
  );
};

export const PageHeader = ({ title, description, count, action }) => (
  <Header className="page-reveal">
    <HeaderCopy>
      <TitleRow><AccentDot aria-hidden="true" /><Title>{title}</Title></TitleRow>
      {description && <Description>{description}</Description>}
    </HeaderCopy>
    {(count !== undefined || action) && <HeaderSide>{count !== undefined && <Count>{count}</Count>}{action}</HeaderSide>}
  </Header>
);

export const SectionHeading = ({ title, meta, extra }) => (
  <SectionRow className="page-reveal">
    <SectionTitle>{title}</SectionTitle>
    {extra || (meta && <SectionMeta>{meta}</SectionMeta>)}
  </SectionRow>
);

export const ProductToolbar = ({ search, filters, actions, className }) => (
  <Toolbar className={`page-reveal ${className || ''}`}>
    <div className="toolbar-search">{search}</div>
    <div className="toolbar-filters">{filters}</div>
    {actions && <div>{actions}</div>}
  </Toolbar>
);

export const StatusTag = ({ children, tone }) => <Tag $tone={tone}>{children}</Tag>;

const normalizeFileType = type => {
  const value = (type || '').toLowerCase();
  if (value === 'folder') return 'folder';
  if (value.includes('pdf')) return 'pdf';
  if (value.includes('wordprocessingml') || value.includes('officedocument.word')) return 'docx';
  if (value.includes('spreadsheetml') || value.includes('officedocument.sheet')) return 'xlsx';
  if (value.includes('presentationml') || value.includes('officedocument.presentation')) return 'pptx';
  if (value.includes('msword')) return 'doc';
  if (value.includes('ms-excel')) return 'xls';
  const subtype = value.includes('/') ? value.split('/').pop() : value;
  return ({ jpeg: 'jpg', 'svg+xml': 'svg', plain: 'txt', markdown: 'md', 'octet-stream': 'file', 'x-zip-compressed': 'zip' })[subtype] || subtype || 'file';
};

export const fileTypeLabel = type => {
  const normalized = normalizeFileType(type);
  if (normalized === 'folder') return '目录';
  if (normalized === 'file') return '文件';
  return normalized.slice(0, 4).toUpperCase();
};

const typeTone = type => {
  const normalized = normalizeFileType(type);
  if (['jpg', 'jpeg', 'png', 'gif', 'webp', 'svg'].includes(normalized)) return 'var(--blue-soft)';
  if (['pdf', 'ppt', 'pptx'].includes(normalized)) return 'var(--coral-soft)';
  if (['zip', 'rar', '7z', 'tar', 'gz'].includes(normalized)) return 'var(--pink-soft)';
  if (['js', 'jsx', 'ts', 'tsx', 'cpp', 'c', 'h', 'py'].includes(normalized)) return 'var(--lime-soft)';
  return 'var(--white)';
};

export const FileTypeBadge = ({ type }) => {
  const normalized = normalizeFileType(type);
  if (normalized === 'folder') return <TypeBadge $tone="var(--blue-soft)" aria-label="目录"><FolderOutlined aria-hidden="true" /></TypeBadge>;
  const label = fileTypeLabel(normalized);
  return <TypeBadge $tone={typeTone(normalized)} title={label}>{label}</TypeBadge>;
};

const imageTypes = new Set(['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'svg', 'avif']);

export const isImageFile = file => imageTypes.has((file?.type || '').toLowerCase()) && Boolean(file?.url);

const PreviewImage = styled.img`
  display: block;
  width: auto;
  max-width: 100%;
  max-height: min(72vh, 760px);
  margin: 0 auto;
  object-fit: contain;
`;

export const ImagePreviewButton = ({ file }) => {
  const [open, setOpen] = useState(false);
  const [failed, setFailed] = useState(false);
  const imageFile = isImageFile(file);

  if (!imageFile) return null;

  return (
    <>
      <Button size="small" icon={<EyeOutlined />} onClick={() => { setFailed(false); setOpen(true); }}>
        预览
      </Button>
      <Modal
        title={file.file_name || file.name || '图片预览'}
        open={open}
        onCancel={() => setOpen(false)}
        footer={null}
        centered
        width={960}
        styles={{ body: { padding: '8px 0 0', textAlign: 'center' } }}
      >
        {failed ? <p>图片加载失败，请稍后重试。</p> : <PreviewImage src={file.url} alt={file.file_name || file.name || '图片预览'} onError={() => setFailed(true)} />}
      </Modal>
    </>
  );
};

export const FilePreview = ({ file, width = '42px', height = '36px' }) => {
  const [failed, setFailed] = useState(false);
  const type = (file?.type || '').toLowerCase();
  const canPreview = imageTypes.has(type) && file?.url && !failed;

  return (
    <PreviewFrame $width={width} $height={height}>
      {canPreview ? (
        <img src={file.url} alt={`${file.file_name || file.name || '文件'} 预览`} loading="lazy" onError={() => setFailed(true)} />
      ) : <FileTypeBadge type={file?.type} />}
    </PreviewFrame>
  );
};

export const GraphicEmpty = ({ title, description, action }) => (
  <EmptyWrap className="page-reveal">
    <EmptyMark aria-hidden="true"><InboxOutlined /></EmptyMark>
    <div><EmptyTitle>{title}</EmptyTitle><EmptyCopy>{description}</EmptyCopy></div>
    {action && <EmptyAction>{action}</EmptyAction>}
  </EmptyWrap>
);

export const UploadOverlay = ({ title, description }) => (
  <DragLayer aria-live="polite">
    <DragBox><div><InboxOutlined /><h2>{title}</h2><p>{description}</p></div></DragBox>
  </DragLayer>
);
