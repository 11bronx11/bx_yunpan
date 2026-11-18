package aisystem

import (
	"context"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/tracing"
	"go.opentelemetry.io/otel/codes"
)

// Never attach source text, questions, grants, credentials or signed URLs.
func stageTrace(ctx context.Context, name string) (context.Context, func(error)) {
	ctx, span := tracing.Tracer().Start(ctx, name)
	return ctx, func(err error) {
		if err != nil {
			span.SetStatus(codes.Error, "AI stage failed")
		}
		span.End()
	}
}
