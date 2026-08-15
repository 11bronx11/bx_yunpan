import React from 'react';
import { Badge, Button, Dropdown } from 'antd';
import { CloudOutlined, CommentOutlined, LogoutOutlined, MenuOutlined, RobotOutlined, SwapOutlined } from '@ant-design/icons';
import { NavLink, useLocation, useNavigate } from 'react-router-dom';
import styled from '@emotion/styled';
import { useAuth } from '../contexts/AuthContext';
import { useTransfers } from '../contexts/TransferContext';

const Header = styled.header`width: min(calc(100% - 64px), 1280px); margin: 20px auto 0; @media (max-width: 900px) { width: calc(100% - 28px); margin-top: 14px; }`;
const Bar = styled.div`display: flex; gap: 16px; align-items: center; min-width: 0; min-height: 60px; padding: 7px 10px 7px 16px; border: 1px solid var(--ink); border-radius: var(--radius-pill); background: rgba(255,255,255,.94); box-shadow: 0 3px 0 var(--ink);`;
const Brand = styled(NavLink)`display: flex; flex: 0 0 auto; gap: 9px; align-items: center; color: var(--ink); font-weight: 700; text-decoration: none;`;
const Mark = styled.span`width: 22px; height: 22px; border: 1px solid var(--ink); border-radius: 50% 44% 50% 36%; background: var(--lime); transform: rotate(-18deg);`;
const Nav = styled.nav`display: flex; flex: 1; justify-content: center; gap: 4px; @media (max-width: 900px) { display: none; }`;
const Item = styled(NavLink)`display: inline-flex; gap: 7px; align-items: center; min-height: 40px; padding: 0 14px; border-radius: var(--radius-pill); color: var(--ink); text-decoration: none; .ant-badge, .ant-badge .anticon { color: inherit; } &:hover { color: var(--ink); background: var(--surface-hover); } &.active, &.active:hover { color: var(--white); background: var(--ink); }`;
const Account = styled.button`min-height: 42px; max-width: 170px; padding: 0 14px; border: 1px solid var(--divider); border-radius: var(--radius-pill); color: var(--ink); background: var(--white); cursor: pointer; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; @media (max-width: 900px) { display: none; }`;
const Mobile = styled(Button)`display: none; width: 44px; height: 44px; @media (max-width: 900px) { display: inline-flex; margin-left: auto; }`;

const NavBar = () => {
  const { user, logout } = useAuth();
  const { activeCount } = useTransfers();
  const location = useLocation();
  const navigate = useNavigate();
  const links = [{ to: '/drive', text: '我的云盘', icon: <CloudOutlined /> }, { to: '/transfers', text: '传输', icon: <Badge count={activeCount} size="small" offset={[7, -3]}><SwapOutlined /></Badge> }, { to: '/ai', text: 'AI 检索', icon: <RobotOutlined /> }, { to: '/ask', text: 'AI 问答', icon: <CommentOutlined /> }];
  const accountMenu = {
    items: [
      { key: 'username', label: user?.username || '当前用户', disabled: true },
      { type: 'divider' },
      { key: 'logout', icon: <LogoutOutlined />, label: '退出登录', danger: true },
    ],
    onClick: ({ key }) => { if (key === 'logout') logout(); },
  };
  const mobileMenu = {
    items: [...links.map(link => ({ key: link.to, icon: link.icon, label: link.text })), { type: 'divider' }, { key: 'logout', icon: <LogoutOutlined />, label: '退出登录', danger: true }],
    selectedKeys: [location.pathname],
    onClick: ({ key }) => { if (key === 'logout') logout(); else navigate(key); },
  };
  return <Header><Bar><Brand to="/drive"><Mark /><span>BX YUNPAN</span></Brand><Nav>{links.map(link => <Item key={link.to} to={link.to} className={location.pathname === link.to ? 'active' : ''}>{link.icon}{link.text}</Item>)}</Nav><Dropdown menu={accountMenu} trigger={['click']} placement="bottomRight"><Account type="button" aria-label="打开账户菜单">{user?.username}</Account></Dropdown><Dropdown menu={mobileMenu} trigger={['click']} placement="bottomRight"><Mobile icon={<MenuOutlined />} aria-label="打开导航" /></Dropdown></Bar></Header>;
};

export default NavBar;
