import React, { forwardRef } from 'react';
import { Input } from 'antd';
import useIMEGuard from '../utils/useIMEGuard';

const withIMEGuard = (Component, name) => {
  const GuardedInput = forwardRef((props, ref) => {
    const { handlers, shouldIgnoreEnter } = useIMEGuard();
    const guardedHandlers = Object.fromEntries(Object.entries(handlers).map(([key, handler]) => [key, event => {
      handler(event);
      props[key]?.(event);
    }]));

    return <Component {...props} {...guardedHandlers} ref={ref} onPressEnter={event => {
      if (!shouldIgnoreEnter(event)) props.onPressEnter?.(event);
    }} />;
  });
  GuardedInput.displayName = name;
  return GuardedInput;
};

const IMEInput = withIMEGuard(Input, 'IMEInput');
IMEInput.TextArea = withIMEGuard(Input.TextArea, 'IMETextArea');
IMEInput.Password = withIMEGuard(Input.Password, 'IMEPassword');

export default IMEInput;
