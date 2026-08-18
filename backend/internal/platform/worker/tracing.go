package worker

import (
	"context"
	"encoding/json"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/tracing"
)

// TracingMiddleware 把 payload 里的 trace context Extract 出来作为 parent，
// 为每个任务建一个 CONSUMER span。
//
// Asynq 没有官方 OTel 中间件，且业务事务与任务消费之间隔着 outbox 表，
// context 无法自然传播——所以 trace 的传播链路是：
// 业务事务 Inject 到 outbox 行 → dispatcher 放进 payload → 这里 Extract。
func TracingMiddleware() asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			ctx = otel.GetTextMapPropagator().Extract(ctx, carrierFromPayload(task.Payload()))

			attributes := []attribute.KeyValue{
				attribute.String("messaging.system", "asynq"),
				attribute.String("messaging.operation", "process"),
				attribute.String("messaging.destination.name", task.Type()),
			}
			if taskID, ok := asynq.GetTaskID(ctx); ok {
				attributes = append(attributes, attribute.String("messaging.message.id", taskID))
			}
			if retried, ok := asynq.GetRetryCount(ctx); ok {
				attributes = append(attributes, attribute.Int("messaging.asynq.retry_count", retried))
			}

			ctx, span := tracing.Tracer().Start(ctx, "asynq.process "+task.Type(),
				trace.WithSpanKind(trace.SpanKindConsumer),
				trace.WithAttributes(attributes...),
			)
			defer span.End()

			err := next.ProcessTask(ctx, task)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return err
		})
	}
}

// carrierFromPayload 读取 payload 里的 _trace 字段。payload 不是 JSON 对象或
// 没有该字段时返回空 carrier，Extract 会退化为无 parent，不影响任务执行。
func carrierFromPayload(payload []byte) outbox.TraceCarrier {
	carrier := outbox.TraceCarrier{}
	if len(payload) == 0 {
		return carrier
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return carrier
	}
	raw, ok := fields[outbox.TracePayloadKey]
	if !ok {
		return carrier
	}
	_ = json.Unmarshal(raw, &carrier)
	return carrier
}
