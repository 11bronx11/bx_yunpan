import React from 'react';
import { Button, ConfigProvider, Form } from 'antd';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import Input from './IMEInput';
import useIMEGuard from '../utils/useIMEGuard';

const enter = (input, options = {}) => {
  const allowed = fireEvent.keyDown(input, { key: 'Enter', keyCode: 13, ...options });
  fireEvent.keyUp(input, { key: 'Enter', keyCode: 13 });
  return allowed;
};

describe.each([
  ['input', Input], ['textarea', Input.TextArea], ['password', Input.Password],
])('%s IME confirmation', (_, Component) => {
  test('keeps composing text and waits for another Enter after composition ends', () => {
    const submit = jest.fn();
    render(<Component aria-label="输入" onPressEnter={submit} />);
    const input = screen.getByLabelText('输入');
    fireEvent.compositionStart(input);
    fireEvent.change(input, { target: { value: 'hello' } });
    // Some IMEs omit isComposing on the key event; track composition too.
    expect(enter(input)).toBe(true);
    expect(submit).not.toHaveBeenCalled();
    expect(input).toHaveValue('hello');
    fireEvent.compositionEnd(input, { data: 'hello' });
    enter(input);
    expect(submit).toHaveBeenCalledTimes(1);
  });

  test.each([
    { isComposing: true },
    { isComposing: false, keyCode: 229 },
  ])('ignores IME key flags even without composition events: %j', options => {
    const submit = jest.fn();
    render(<Component aria-label="输入" onPressEnter={submit} />);
    const input = screen.getByLabelText('输入');
    expect(enter(input, options)).toBe(true);
    expect(submit).not.toHaveBeenCalled();
    enter(input);
    expect(submit).toHaveBeenCalledTimes(1);
  });
});

const GuardedForm = ({ submit }) => {
  const ime = useIMEGuard();
  return <ConfigProvider theme={{ token: { motion: false } }}>
    <Form aria-label="认证" {...ime.handlers} onSubmitCapture={ime.onSubmitCapture} onFinish={submit}>
      <Form.Item name="username"><Input aria-label="用户名" /></Form.Item>
      <Button htmlType="submit">登录</Button>
    </Form>
  </ConfigProvider>;
};

test('cancels implicit form submission without canceling IME confirmation', async () => {
  const submit = jest.fn();
  render(<GuardedForm submit={submit} />);
  const input = screen.getByLabelText('用户名');
  const form = screen.getByRole('form', { name: '认证' });
  fireEvent.compositionStart(input);
  fireEvent.change(input, { target: { value: 'hello' } });
  expect(fireEvent.keyDown(input, { key: 'Enter', isComposing: true })).toBe(true);
  // Composition can end between keydown and the form's default submission.
  fireEvent.compositionEnd(input, { data: 'hello' });
  fireEvent.submit(form);
  expect(submit).not.toHaveBeenCalled();
  fireEvent.keyUp(input, { key: 'Enter' });
  fireEvent.keyDown(input, { key: 'Enter' });
  fireEvent.submit(form);
  await waitFor(() => expect(submit).toHaveBeenCalledWith({ username: 'hello' }));
});

test('allows an explicit button click after IME confirmation consumed the keyup', async () => {
  const submit = jest.fn();
  render(<GuardedForm submit={submit} />);
  const input = screen.getByLabelText('用户名');
  fireEvent.compositionStart(input);
  fireEvent.change(input, { target: { value: 'hello' } });
  fireEvent.keyDown(input, { key: 'Enter', isComposing: true });
  fireEvent.compositionEnd(input, { data: 'hello' });
  const button = screen.getByRole('button', { name: /登\s*录/ });
  fireEvent.pointerDown(button);
  fireEvent.click(button);
  await waitFor(() => expect(submit).toHaveBeenCalledWith({ username: 'hello' }));
});
