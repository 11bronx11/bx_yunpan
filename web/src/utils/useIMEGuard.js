import { useRef } from 'react';

const isComposing = event => event.isComposing || event.nativeEvent?.isComposing
  || event.keyCode === 229 || event.nativeEvent?.keyCode === 229;

export default function useIMEGuard() {
  const composing = useRef(false);
  const confirming = useRef(false);

  const shouldIgnoreEnter = event => composing.current || confirming.current || isComposing(event);
  const handlers = {
    onCompositionStart: () => { composing.current = true; },
    onCompositionEnd: () => { composing.current = false; },
    onKeyDownCapture: event => {
      confirming.current = event.key === 'Enter' && Boolean(composing.current || isComposing(event));
    },
    onKeyUpCapture: event => { if (event.key === 'Enter') confirming.current = false; },
    onBlur: () => { composing.current = false; confirming.current = false; },
    onPointerDownCapture: () => { confirming.current = false; },
  };

  const onSubmitCapture = event => {
    if (!confirming.current) return;
    // Cancel the form submission, leaving the IME's Enter default intact.
    event.preventDefault();
    event.stopPropagation();
  };

  return { handlers, shouldIgnoreEnter, onSubmitCapture };
}
