package outbox

import (
	"context"

	"go.opentelemetry.io/otel"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
)

// v2EventsContextKey is intentionally private: only this package can decide
// which event types belong to the compatibility window.
type v2EventsContextKey struct{}

type searchEnabledContextKey struct{}

func WithSearchEnabled(ctx context.Context, enabled bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, searchEnabledContextKey{}, enabled)
}

func SearchEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(searchEnabledContextKey{}).(bool)
	return enabled
}

// WithV2EventsEnabled carries the rollout flag with the request or worker
// context. Keeping it in context makes the flag observable and testable without
// introducing mutable process-global configuration.
func WithV2EventsEnabled(ctx context.Context, enabled bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, v2EventsContextKey{}, enabled)
}

// V2EventsEnabled reports the rollout state. The safe default is false so a
// context that crossed an old process boundary cannot accidentally enable new
// domain events.
func V2EventsEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(v2EventsContextKey{}).(bool)
	return enabled
}

// IsV2Event identifies events introduced by P0.3. Existing command and object
// lifecycle events remain writable during the compatibility window.
func IsV2Event(eventType string) bool {
	switch eventType {
	case eventcontract.EventFileUpdatedV1,
		eventcontract.EventFileDeletedV1,
		eventcontract.EventFolderCreatedV1,
		eventcontract.EventFolderRenamedV1,
		eventcontract.EventFolderMovedV1,
		eventcontract.EventFolderDeletedV1,
		eventcontract.EventShareCreatedV1,
		eventcontract.EventShareUpdatedV1,
		eventcontract.EventShareRevokedV1:
		return true
	default:
		return false
	}
}

// AddTyped constructs, validates, and writes a typed envelope in the caller's
// transaction. New P0.3 events are skipped while the rollout flag is closed;
// the business transaction still commits.
func AddTyped(ctx context.Context, tx *gorm.DB, input eventcontract.EnvelopeInput) error {
	if input.Trace == nil && ctx != nil {
		carrier := TraceCarrier{}
		otel.GetTextMapPropagator().Inject(ctx, carrier)
		if carrier["traceparent"] != "" || carrier["tracestate"] != "" {
			input.Trace = &eventcontract.TraceContext{Traceparent: carrier["traceparent"], Tracestate: carrier["tracestate"]}
		}
	}
	envelope, err := eventcontract.New(input)
	if err != nil {
		return err
	}
	return AddEnvelopeContext(ctx, tx, envelope)
}

// AddEnvelopeContext is the context-aware writer used by service boundaries.
// AddEnvelope remains unconditional for legacy callers during expand/contract.
func AddEnvelopeContext(ctx context.Context, tx *gorm.DB, envelope eventcontract.Envelope) error {
	if IsV2Event(envelope.EventType) && !V2EventsEnabled(ctx) {
		return nil
	}
	return addEnvelopeWithSearch(tx, envelope, SearchEnabled(ctx))
}
