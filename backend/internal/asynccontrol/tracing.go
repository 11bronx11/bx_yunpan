package asynccontrol

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func BeginTrace(ctx context.Context, t Task, phase string) (context.Context, trace.Span) {
	carrier := propagation.MapCarrier{}
	_ = json.Unmarshal(t.Trace, &carrier)
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	return otel.Tracer("bx/async").Start(ctx, "async."+phase+" "+t.Kind, trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(attribute.String("async.task_id", t.ID.String()), attribute.String("async.semantic_key", t.SemanticKey), attribute.String("async.resource", t.ResourceKey), attribute.Int64("async.generation", t.Generation)))
}
