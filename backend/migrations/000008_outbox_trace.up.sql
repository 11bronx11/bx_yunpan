-- Asynq 没有官方 OTel 中间件，且业务事务与任务消费之间隔着 outbox 表，
-- context 无法自然传播。把 span context 序列化存进 outbox 行，与业务数据
-- 同一事务提交：outbox 行不丢，trace 上下文就不丢。
ALTER TABLE outbox_events ADD COLUMN trace_context jsonb;

COMMENT ON COLUMN outbox_events.trace_context IS
    'W3C traceparent/tracestate injected at business-transaction time so async consumers can continue the trace.';
