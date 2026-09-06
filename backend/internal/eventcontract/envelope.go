// Package eventcontract contains the versioned messages exchanged by the
// business transaction and asynchronous consumers.  It deliberately has no
// dependency on the outbox implementation so producers and consumers can
// share the contract without importing a transport.
package eventcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MessageKind distinguishes a fact from work requested from a consumer.
// Domain events are safe to retain and replay; commands describe an
// execution request and are normally routed to one short-lived queue.
type MessageKind string

const (
	MessageKindDomainEvent MessageKind = "domain_event"
	MessageKindCommand     MessageKind = "command"
)

// TraceContext is transport metadata.  It is intentionally kept outside the
// business payload so payload structs do not need a private _trace field.
type TraceContext struct {
	Traceparent string `json:"traceparent,omitempty"`
	Tracestate  string `json:"tracestate,omitempty"`
}

// Envelope is the stable wire representation of an event or command.
//
// AggregateVersion is a pointer because rows written by the legacy outbox can
// legitimately have an unknown version.  New envelopes created with New
// always carry a positive version; preserving nil on decode prevents callers
// from fabricating a historical version.
type Envelope struct {
	EventID          uuid.UUID       `json:"event_id"`
	EventType        string          `json:"event_type"`
	MessageKind      MessageKind     `json:"message_kind"`
	EventVersion     int             `json:"event_version"`
	AggregateType    string          `json:"aggregate_type"`
	AggregateID      uuid.UUID       `json:"aggregate_id"`
	AggregateVersion *int64          `json:"aggregate_version,omitempty"`
	Producer         string          `json:"producer"`
	OccurredAt       time.Time       `json:"occurred_at"`
	Trace            *TraceContext   `json:"trace,omitempty"`
	Payload          json.RawMessage `json:"payload"`
}

var (
	ErrUnknownEventType = errors.New("unknown event type")
	ErrInvalidEnvelope  = errors.New("invalid event envelope")
	ErrInvalidPayload   = errors.New("invalid event payload")
)

// EnvelopeInput contains the fields needed to construct a new envelope.
// MessageKind, EventVersion, AggregateType, EventID, and OccurredAt may be
// omitted when the registry/default clock should provide them.
type EnvelopeInput struct {
	EventID          uuid.UUID
	EventType        string
	MessageKind      MessageKind
	EventVersion     int
	AggregateType    string
	AggregateID      uuid.UUID
	AggregateVersion int64
	Producer         string
	OccurredAt       time.Time
	Trace            *TraceContext
	Payload          any
}

// New constructs and validates a v1 envelope using the registered contract.
// Defaults are deliberately derived from the registry, so a caller cannot
// silently route a known event as an arbitrary command or aggregate.
func New(input EnvelopeInput) (Envelope, error) {
	eventType := strings.TrimSpace(input.EventType)
	definition, ok := Lookup(eventType)
	if !ok {
		return Envelope{}, fmt.Errorf("%w: %q", ErrUnknownEventType, eventType)
	}
	if input.EventID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return Envelope{}, fmt.Errorf("generate event id: %w", err)
		}
		input.EventID = id
	}
	if input.EventVersion == 0 {
		input.EventVersion = definition.EventVersion
	}
	if input.MessageKind == "" {
		input.MessageKind = definition.MessageKind
	}
	if strings.TrimSpace(input.AggregateType) == "" {
		input.AggregateType = definition.AggregateType
	}
	if input.OccurredAt.IsZero() {
		input.OccurredAt = time.Now().UTC()
	}
	encoded, err := json.Marshal(input.Payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	version := input.AggregateVersion
	envelope := Envelope{
		EventID:          input.EventID,
		EventType:        eventType,
		MessageKind:      input.MessageKind,
		EventVersion:     input.EventVersion,
		AggregateType:    strings.TrimSpace(input.AggregateType),
		AggregateID:      input.AggregateID,
		AggregateVersion: &version,
		Producer:         strings.TrimSpace(input.Producer),
		OccurredAt:       input.OccurredAt.UTC(),
		Trace:            input.Trace,
		Payload:          encoded,
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// NewEnvelope is a descriptive alias for New, useful at transport call
// sites where the distinction from a payload is important.
func NewEnvelope(input EnvelopeInput) (Envelope, error) { return New(input) }

// Validate checks both envelope metadata and the registered typed payload.
// Unknown JSON fields are accepted to preserve forward compatibility for
// additive v1 changes.
func (e Envelope) Validate() error {
	if e.EventID == uuid.Nil {
		return fmt.Errorf("%w: event_id is required", ErrInvalidEnvelope)
	}
	eventType := strings.TrimSpace(e.EventType)
	definition, ok := Lookup(eventType)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownEventType, eventType)
	}
	if e.MessageKind != definition.MessageKind {
		return fmt.Errorf("%w: message_kind %q does not match %q", ErrInvalidEnvelope, e.MessageKind, definition.MessageKind)
	}
	if e.EventVersion != definition.EventVersion {
		return fmt.Errorf("%w: event_version %d does not match %d", ErrInvalidEnvelope, e.EventVersion, definition.EventVersion)
	}
	if strings.TrimSpace(e.AggregateType) == "" {
		return fmt.Errorf("%w: aggregate_type is required", ErrInvalidEnvelope)
	}
	if definition.AggregateType != "" && e.AggregateType != definition.AggregateType {
		return fmt.Errorf("%w: aggregate_type %q does not match %q", ErrInvalidEnvelope, e.AggregateType, definition.AggregateType)
	}
	if e.AggregateID == uuid.Nil {
		return fmt.Errorf("%w: aggregate_id is required", ErrInvalidEnvelope)
	}
	if e.AggregateVersion != nil && *e.AggregateVersion <= 0 {
		return fmt.Errorf("%w: aggregate_version must be positive", ErrInvalidEnvelope)
	}
	if strings.TrimSpace(e.Producer) == "" {
		return fmt.Errorf("%w: producer is required", ErrInvalidEnvelope)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("%w: occurred_at is required", ErrInvalidEnvelope)
	}
	if e.Trace != nil && e.Trace.Traceparent == "" && e.Trace.Tracestate == "" {
		return fmt.Errorf("%w: trace must contain traceparent or tracestate", ErrInvalidEnvelope)
	}
	if len(bytes.TrimSpace(e.Payload)) == 0 || bytes.Equal(bytes.TrimSpace(e.Payload), []byte("null")) {
		return fmt.Errorf("%w: payload is required", ErrInvalidEnvelope)
	}
	if _, err := DecodePayload(eventType, e.Payload); err != nil {
		return err
	}
	return nil
}

// MarshalJSON prevents an invalid contract from being emitted to a queue or
// persisted as a new outbox record.  Legacy rows with a nil aggregate version
// can still be decoded and inspected, but New is required for new writes.
func (e Envelope) MarshalJSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	type envelopeAlias Envelope
	return json.Marshal(envelopeAlias(e))
}

// Decode parses and validates a complete envelope.  Additional top-level
// fields are ignored for additive forward compatibility.
func Decode(data []byte) (Envelope, error) {
	var envelope Envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode event envelope: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Envelope{}, fmt.Errorf("%w: multiple JSON values", ErrInvalidEnvelope)
		}
		return Envelope{}, fmt.Errorf("decode event envelope: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// DecodePayload decodes a payload into the registered v1 struct.  The return
// value is a pointer to the named payload type, allowing callers to type
// assert without falling back to map[string]any.
func DecodePayload(eventType string, data []byte) (any, error) {
	definition, ok := Lookup(strings.TrimSpace(eventType))
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownEventType, eventType)
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, fmt.Errorf("%w: %s is empty", ErrInvalidPayload, eventType)
	}
	payload := definition.NewPayload()
	if err := json.Unmarshal(data, payload); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %w", ErrInvalidPayload, eventType, err)
	}
	if validator, ok := payload.(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalidPayload, eventType, err)
		}
	}
	return payload, nil
}

// PayloadValue is a convenience method around DecodePayload.
func (e Envelope) PayloadValue() (any, error) { return DecodePayload(e.EventType, e.Payload) }
