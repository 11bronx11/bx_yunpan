import { App as AntApp, Button, Form, Input } from 'antd';
import { LockOutlined, MailOutlined, UserOutlined } from '@ant-design/icons';
import { useGSAP } from '@gsap/react';
import { gsap } from 'gsap';
import { useNavigate } from 'react-router-dom';
import { useAuth } from '../contexts/AuthContext';
import { loginUser, registerUser } from '../services/auth';
import styled from '@emotion/styled';
import React, { useRef, useState } from 'react';

gsap.registerPlugin(useGSAP);

const AuthCanvas = styled.main`
  --ink: #080808;
  --paper: #f7f7f2;
  --lime: #c6ff00;
  --blue: #087fc3;
  --pink: #ff2864;
  --coral: #ff704d;
  position: relative;
  isolation: isolate;
  min-height: 100dvh;
  overflow: hidden;
  color: var(--ink);
  background-color: var(--paper);
  background-image:
    linear-gradient(rgba(8, 8, 8, 0.08) 1px, transparent 1px),
    linear-gradient(90deg, rgba(8, 8, 8, 0.08) 1px, transparent 1px);
  background-size: 28px 28px;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;

  &::after {
    position: absolute;
    inset: 0;
    z-index: 0;
    pointer-events: none;
    content: "";
    opacity: 0.04;
    background-image: radial-gradient(var(--ink) 0.6px, transparent 0.7px);
    background-size: 5px 5px;
  }

  @media (max-width: 760px) {
    overflow-x: hidden;
    overflow-y: visible;
    background-size: 24px 24px;
  }
`;

const Topline = styled.header`
  position: relative;
  z-index: 6;
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  padding: 26px 32px;
  font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
  font-size: 11px;
  font-weight: 800;
  line-height: 1.1;
  letter-spacing: 0.08em;

  @media (max-width: 760px) {
    padding: 18px 20px;
    font-size: 9px;
  }
`;

const Brand = styled.div`
  display: flex;
  align-items: center;
  gap: 9px;
  text-transform: uppercase;

  &::before {
    width: 18px;
    height: 18px;
    content: "";
    border: 2px solid var(--ink);
    border-radius: 50% 44% 50% 36%;
    background: var(--lime);
    transform: rotate(-18deg);
  }
`;

const Issue = styled.div`
  display: flex;
  gap: 8px;
  align-items: center;

  &::before {
    width: 8px;
    height: 8px;
    content: "";
    border-radius: 50%;
    background: var(--pink);
  }
`;

const Hero = styled.section`
  position: absolute;
  z-index: 3;
  top: clamp(116px, 15vh, 168px);
  left: clamp(20px, 5vw, 78px);
  width: min(67vw, 980px);
  pointer-events: none;

  @media (max-width: 960px) {
    top: 138px;
    width: 64vw;
  }

  @media (max-width: 760px) {
    position: relative;
    top: auto;
    left: auto;
    width: auto;
    margin: 58px 20px 0;
  }
`;

const HeroMask = styled.div`
  overflow: hidden;
`;

const HeroLine = styled.h1`
  margin: 0;
  color: var(--ink);
  font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
  font-size: clamp(72px, 10.6vw, 178px);
  font-stretch: condensed;
  font-weight: 900;
  line-height: 0.76;
  letter-spacing: -0.075em;
  transform: scaleX(0.88);
  transform-origin: left center;
  white-space: nowrap;

  &:last-child {
    margin-top: 0.1em;
    padding-left: clamp(32px, 12vw, 190px);
  }

  @media (max-width: 760px) {
    font-size: clamp(58px, 18.3vw, 86px);
    line-height: 0.8;
    white-space: normal;

    &:last-child {
      padding-left: 12vw;
    }
  }
`;

const HeroMeta = styled.p`
  position: relative;
  z-index: 4;
  margin: clamp(24px, 4vh, 54px) 0 0 clamp(4px, 4vw, 58px);
  font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
  font-size: clamp(11px, 1.1vw, 14px);
  font-weight: 800;
  letter-spacing: 0.04em;
  line-height: 1.25;
  text-transform: uppercase;

  @media (max-width: 760px) {
    margin-top: 28px;
    margin-left: 2px;
    font-size: 10px;
  }
`;

const Blob = styled.div`
  position: absolute;
  z-index: 2;
  pointer-events: none;
  background: ${({ $color }) => $color};
  border: 2px solid var(--ink);
  will-change: transform;
`;

const BlueBlob = styled(Blob)`
  top: 39%;
  left: -9%;
  width: min(34vw, 510px);
  height: min(17vw, 255px);
  border-radius: 58% 42% 52% 34% / 60% 34% 66% 40%;
  transform: rotate(-17deg);

  &::before,
  &::after {
    position: absolute;
    top: 62%;
    width: 27px;
    height: 38px;
    content: "";
    border-radius: 50%;
    background: var(--ink);
  }

  &::before { left: 58%; }
  &::after { left: calc(58% + 50px); }

  @media (max-width: 760px) {
    top: 277px;
    left: -64px;
    width: 230px;
    height: 116px;
  }
`;

const LimeBlob = styled(Blob)`
  top: 17%;
  right: 25%;
  width: min(16vw, 238px);
  height: min(16vw, 238px);
  border-radius: 43% 57% 65% 35% / 40% 40% 60% 60%;
  transform: rotate(24deg);

  @media (max-width: 960px) { right: 16%; }
  @media (max-width: 760px) {
    top: 305px;
    right: 23px;
    width: 100px;
    height: 100px;
  }
`;

const CoralBlob = styled(Blob)`
  right: -56px;
  bottom: -74px;
  width: min(25vw, 380px);
  height: min(19vw, 286px);
  border-radius: 64% 36% 38% 62% / 42% 59% 41% 58%;
  transform: rotate(-18deg);

  @media (max-width: 760px) {
    right: -72px;
    bottom: -36px;
    width: 200px;
    height: 150px;
  }
`;

const PinkDot = styled(Blob)`
  top: 31%;
  right: 46%;
  width: 42px;
  height: 42px;
  border-radius: 50%;

  @media (max-width: 760px) { display: none; }
`;

const PanelWrap = styled.section`
  position: relative;
  z-index: 5;
  width: min(440px, calc(100vw - 56px));
  margin-left: auto;
  margin-right: clamp(36px, 7vw, 132px);
  padding-top: ${({ $register }) => ($register ? 'clamp(42px, 6vh, 64px)' : 'clamp(158px, 18vh, 210px)')};

  @media (max-width: 960px) {
    margin-right: 32px;
    padding-top: ${({ $register }) => ($register ? '310px' : '386px')};
  }

  @media (max-width: 760px) {
    width: auto;
    margin: 70px 20px 0;
    padding: 0 0 84px;
  }
`;

const Panel = styled.div`
  position: relative;
  overflow: hidden;
  padding: 24px 26px 20px;
  border: 2px solid var(--ink);
  border-radius: 28px;
  background: #fff;
  box-shadow: 8px 8px 0 var(--ink);

  @media (max-width: 760px) {
    padding: 20px 18px 18px;
    border-radius: 24px;
    box-shadow: 6px 6px 0 var(--ink);
  }
`;

const ModeSwitch = styled.div`
  position: relative;
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 3px;
  padding: 3px;
  border: 2px solid var(--ink);
  border-radius: 999px;
  background: #fff;
`;

const ModeIndicator = styled.span`
  position: absolute;
  top: 3px;
  bottom: 3px;
  left: 3px;
  width: calc(50% - 4px);
  border-radius: 999px;
  background: var(--ink);
`;

const ModeButton = styled.button`
  position: relative;
  z-index: 1;
  min-height: 38px;
  padding: 0 9px;
  border: 0;
  border-radius: 999px;
  color: ${({ $active }) => ($active ? '#fff' : 'var(--ink)')};
  background: transparent;
  cursor: pointer;
  font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
  font-size: 10px;
  font-weight: 800;
  letter-spacing: 0.035em;
  text-transform: uppercase;

  &:focus-visible { outline: 3px solid var(--pink); outline-offset: 2px; }
`;

const PanelTitle = styled.h2`
  margin: 23px 0 6px;
  font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
  font-size: clamp(28px, 3vw, 38px);
  font-weight: 900;
  line-height: 0.9;
  letter-spacing: -0.055em;
  text-transform: uppercase;
`;

const PanelIntro = styled.p`
  margin: 0 0 21px;
  color: #4c4c47;
  font-size: 13px;
  line-height: 1.5;
`;

const StyledForm = styled(Form)`
  .ant-form-item { margin-bottom: 15px; }
  .ant-form-item-label { padding-bottom: 6px; }
  .ant-form-item-label > label {
    height: auto;
    color: var(--ink);
    font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
    font-size: 10px;
    font-weight: 800;
    letter-spacing: 0.09em;
    text-transform: uppercase;
  }
  .ant-form-item-explain-error {
    margin-top: 5px;
    color: #be123c;
    font-size: 12px;
    font-weight: 600;
  }
  .ant-input-affix-wrapper,
  .ant-input {
    min-height: 54px;
    border: 2px solid var(--ink);
    border-radius: 999px;
    background: #fff;
    box-shadow: none;
    color: var(--ink);
    font-size: 15px;
  }
  .ant-input-affix-wrapper { padding: 0 16px; }
  .ant-input-affix-wrapper > input.ant-input { min-height: 48px; padding: 0 8px; border: 0; }
  .ant-input-prefix { margin-right: 8px; color: var(--ink); }
  .ant-input-password-icon { color: var(--ink); }
  .ant-input-affix-wrapper:hover,
  .ant-input:hover { border-color: var(--ink); }
  .ant-input-affix-wrapper:focus,
  .ant-input-affix-wrapper-focused,
  .ant-input:focus {
    border-color: var(--ink);
    box-shadow: 4px 4px 0 var(--lime);
  }
  .ant-form-item-has-error .ant-input-affix-wrapper,
  .ant-form-item-has-error .ant-input {
    border-color: #be123c;
    box-shadow: 3px 3px 0 #ffb3c7;
  }
`;

const RegisterGrid = styled.div`
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 0 12px;

  @media (max-width: 480px) { grid-template-columns: 1fr; gap: 0; }
`;

const SubmitButton = styled(Button)`
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
  height: 56px;
  margin-top: 3px;
  padding: 0 18px 0 21px;
  border: 2px solid var(--ink) !important;
  border-radius: 999px;
  color: var(--ink) !important;
  background: var(--lime) !important;
  box-shadow: 0 0 0 var(--ink);
  font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
  font-size: 12px;
  font-weight: 800;
  letter-spacing: 0.07em;
  text-transform: uppercase;
  transition: background 180ms ease, color 180ms ease, box-shadow 180ms ease;
  will-change: transform;

  &:hover,
  &:focus {
    color: #fff !important;
    background: var(--ink) !important;
    box-shadow: 4px 4px 0 var(--pink);
  }
  &:focus-visible { outline: 3px solid var(--blue); outline-offset: 3px; }
  .anticon { font-size: 19px; }
`;

const PanelFooter = styled.div`
  display: flex;
  justify-content: space-between;
  gap: 12px;
  margin-top: 16px;
  padding-top: 13px;
  border-top: 1px solid var(--ink);
  font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
  font-size: 9px;
  font-weight: 800;
  letter-spacing: 0.05em;
  line-height: 1.25;
  text-transform: uppercase;
`;

const SideNote = styled.div`
  position: absolute;
  z-index: 4;
  bottom: 29px;
  left: 33px;
  writing-mode: vertical-rl;
  transform: rotate(180deg);
  font-family: "Arial Black", "Helvetica Neue", Arial, sans-serif;
  font-size: 9px;
  font-weight: 800;
  letter-spacing: 0.12em;

  @media (max-width: 760px) { display: none; }
`;

const Login = () => {
  const { message } = AntApp.useApp();
  const navigate = useNavigate();
  const { login } = useAuth();
  const [isRegister, setIsRegister] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [form] = Form.useForm();
  const canvasRef = useRef(null);
  const panelRef = useRef(null);
  const indicatorRef = useRef(null);
  const submitRef = useRef(null);
  const prefersReducedMotion = () => window.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches === true;

  useGSAP(() => {
    if (prefersReducedMotion()) return undefined;

    const timeline = gsap.timeline({ defaults: { ease: 'power3.out' } });
    timeline
      .from('.auth-brand', { autoAlpha: 0, y: 12, duration: 0.35 })
      .from('.hero-reveal', { yPercent: 110, duration: 0.72, stagger: 0.1 }, 0.1)
      .from('.auth-blob', { autoAlpha: 0, scale: 0.6, rotation: '+=18', duration: 0.62, stagger: 0.08 }, 0.23)
      .from(panelRef.current, { autoAlpha: 0, y: 42, rotation: 1.4, duration: 0.56 }, 0.42)
      .from('.auth-micro', { autoAlpha: 0, y: 8, duration: 0.32, stagger: 0.06 }, 0.58);

    gsap.to('.blob-blue', { y: 12, rotation: -14, duration: 8.5, ease: 'sine.inOut', repeat: -1, yoyo: true });
    gsap.to('.blob-lime', { y: -11, rotation: 28, scale: 1.03, duration: 6.8, ease: 'sine.inOut', repeat: -1, yoyo: true, delay: 0.7 });
    gsap.to('.blob-coral', { y: -14, rotation: -14, duration: 10.5, ease: 'sine.inOut', repeat: -1, yoyo: true, delay: 0.3 });

    return undefined;
  }, { scope: canvasRef });

  useGSAP(() => {
    gsap.to(indicatorRef.current, {
      xPercent: isRegister ? 100 : 0,
      duration: 0.28,
      ease: 'power3.out',
    });
    gsap.fromTo(panelRef.current, { rotation: isRegister ? -0.8 : 0.8 }, { rotation: 0, duration: 0.34, ease: 'power3.out' });
    gsap.from('.mode-field', { autoAlpha: 0, y: 10, duration: 0.28, stagger: 0.045, ease: 'power3.out' });
  }, { dependencies: [isRegister], scope: canvasRef, revertOnUpdate: false });

  const onFinish = async (values) => {
    setSubmitting(true);
    try {
      const session = isRegister
        ? await registerUser(values)
        : await loginUser(values.username, values.password);
      login(session);
      message.success(isRegister ? '空间创建成功' : '登录成功');
      navigate('/drive');
    } catch (error) {
      message.error(error.message || (isRegister ? '注册失败' : '登录失败'));
    } finally {
      setSubmitting(false);
    }
  };

  const switchMode = (nextMode) => {
    if (nextMode === isRegister) return;
    setIsRegister(nextMode);
    form.resetFields();
  };

  const handleMagneticMove = (event) => {
    if (prefersReducedMotion() || !submitRef.current) return;
    const bounds = event.currentTarget.getBoundingClientRect();
    const x = ((event.clientX - bounds.left) / bounds.width - 0.5) * 7;
    const y = ((event.clientY - bounds.top) / bounds.height - 0.5) * 5;
    gsap.to(submitRef.current, { x, y, duration: 0.22, ease: 'power3.out', overwrite: 'auto' });
  };

  const resetMagneticButton = () => {
    if (submitRef.current) gsap.to(submitRef.current, { x: 0, y: 0, duration: 0.45, ease: 'elastic.out(1, 0.45)' });
  };

  return (
    <AuthCanvas ref={canvasRef}>
      <Topline className="auth-micro">
        <Brand className="auth-brand">AI 云存储 <span> / AI CLOUD STORAGE</span></Brand>
        <Issue>01 / AUTH</Issue>
      </Topline>

      <Hero aria-label="Your files. Your cloud.">
        <HeroMask><HeroLine className="hero-reveal">YOUR FILES.</HeroLine></HeroMask>
        <HeroMask><HeroLine className="hero-reveal">YOUR CLOUD.</HeroLine></HeroMask>
        <HeroMeta className="auth-micro">STORE. &nbsp; SEARCH. &nbsp; SHARE.<br />上传、管理、分享，并用 AI 找回每一份文件。</HeroMeta>
      </Hero>

      <BlueBlob className="auth-blob blob-blue" $color="#087FC3" aria-hidden="true" />
      <LimeBlob className="auth-blob blob-lime" $color="#C6FF00" aria-hidden="true" />
      <PinkDot className="auth-blob" $color="#FF2864" aria-hidden="true" />
      <CoralBlob className="auth-blob blob-coral" $color="#FF704D" aria-hidden="true" />
      <SideNote className="auth-micro">PRIVATE STORAGE / SEMANTIC SEARCH / 2026</SideNote>

      <PanelWrap $register={isRegister}>
        <Panel ref={panelRef}>
          <ModeSwitch aria-label="认证模式">
            <ModeIndicator ref={indicatorRef} />
            <ModeButton type="button" $active={!isRegister} aria-pressed={!isRegister} onClick={() => switchMode(false)}>Login</ModeButton>
            <ModeButton type="button" $active={isRegister} aria-pressed={isRegister} onClick={() => switchMode(true)}>Create account</ModeButton>
          </ModeSwitch>

          <PanelTitle>{isRegister ? 'Create your space.' : 'Welcome back.'}</PanelTitle>
          <PanelIntro>{isRegister ? '建立你的私有文件空间，从这里开始。' : '回到你的文件、记忆与云端空间。'}</PanelIntro>

          <StyledForm
          form={form}
          name="login"
          onFinish={onFinish}
          autoComplete="on"
          requiredMark={false}
          layout="vertical"
          size="large"
          >
          {isRegister && (
            <RegisterGrid>
              <Form.Item
                name="email"
                className="mode-field"
                label="Email"
                rules={[
                  { required: true, message: '请输入邮箱！' },
                  { type: 'email', message: '请输入有效的邮箱地址！' }
                ]}
              >
                <Input prefix={<MailOutlined />} placeholder="name@example.com" autoComplete="email" inputMode="email" />
              </Form.Item>

              <Form.Item
                name="username"
                className="mode-field"
                label="Username"
                rules={[{ required: true, message: '请输入用户名！' }]}
              >
                <Input prefix={<UserOutlined />} placeholder="用户名" autoComplete="username" />
              </Form.Item>
            </RegisterGrid>
          )}

          {!isRegister && (
            <Form.Item name="username" className="mode-field" label="Username" rules={[{ required: true, message: '请输入用户名！' }]}>
              <Input prefix={<UserOutlined />} placeholder="用户名" autoComplete="username" />
            </Form.Item>
          )}

          <Form.Item
            name="password"
            className="mode-field"
            label="Password"
            rules={[{ required: true, message: '请输入密码！' }]}
          >
            <Input.Password prefix={<LockOutlined />} placeholder="密码" autoComplete={isRegister ? 'new-password' : 'current-password'} />
          </Form.Item>

          {isRegister && (
            <Form.Item
              name="confirmPassword"
              className="mode-field"
              label="Confirm password"
              dependencies={['password']}
              rules={[
                { required: true, message: '请确认密码！' },
                ({ getFieldValue }) => ({
                  validator(_, value) {
                    if (!value || getFieldValue('password') === value) {
                      return Promise.resolve();
                    }
                    return Promise.reject(new Error('两次输入的密码不一致！'));
                  },
                }),
              ]}
            >
              <Input.Password prefix={<LockOutlined />} placeholder="再次输入密码" autoComplete="new-password" />
            </Form.Item>
          )}

          <Form.Item>
            <SubmitButton
              ref={submitRef}
              type="primary"
              htmlType="submit"
              loading={submitting}
              onMouseMove={handleMagneticMove}
              onMouseLeave={resetMagneticButton}
            >
              <span>{submitting ? (isRegister ? 'Creating...' : 'Entering...') : (isRegister ? 'Create account' : 'Enter cloud')}</span>
              {!submitting && <span aria-hidden="true">↗</span>}
            </SubmitButton>
          </Form.Item>
          </StyledForm>
          <PanelFooter><span>Secure storage</span><span>AI search ready</span></PanelFooter>
        </Panel>
      </PanelWrap>
    </AuthCanvas>
  );
};

export default Login;
