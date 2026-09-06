package eventcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestV1PayloadFixtureCompatibility(t *testing.T) {
	raw, err := os.ReadFile("testdata/file.created.v1.payload.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := DecodePayload(EventFileCreatedV1, raw)
	if err != nil {
		t.Fatalf("decode legacy payload: %v", err)
	}
	payload, ok := value.(*FileCreatedV1)
	if !ok {
		t.Fatalf("payload type = %T", value)
	}
	if payload.OwnerID != uuid.MustParse("11111111-1111-4111-8111-111111111111") ||
		payload.FileEntryID != uuid.MustParse("22222222-2222-4222-8222-222222222222") ||
		payload.ObjectID != uuid.MustParse("33333333-3333-4333-8333-333333333333") {
		t.Fatalf("decoded payload = %+v", payload)
	}
}

func TestEnvelopeJSONGolden(t *testing.T) {
	eventID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	aggregateID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	version := int64(4)
	payload := FileCreatedV1{
		OwnerID:     uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		FileEntryID: aggregateID,
		ObjectID:    uuid.MustParse("33333333-3333-4333-8333-333333333333"),
	}
	event, err := New(EnvelopeInput{
		EventID:          eventID,
		EventType:        EventFileCreatedV1,
		AggregateID:      aggregateID,
		AggregateVersion: version,
		Producer:         "core-api",
		OccurredAt:       time.Date(2026, 9, 4, 0, 0, 0, 123000000, time.UTC),
		Trace:            &TraceContext{Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		Payload:          payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("testdata/file.created.v1.envelope.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(actual), bytes.TrimSpace(expected)) {
		t.Fatalf("golden mismatch\nactual:\n%s\nexpected:\n%s", actual, expected)
	}
	decoded, err := Decode(actual)
	if err != nil {
		t.Fatalf("decode marshaled envelope: %v", err)
	}
	if decoded.AggregateVersion == nil || *decoded.AggregateVersion != version {
		t.Fatalf("aggregate version = %v", decoded.AggregateVersion)
	}
}

func TestEnvelopeRejectsUnknownRouteAndMismatchedKind(t *testing.T) {
	base := EnvelopeInput{
		EventType:        EventObjectGCRequestedV1,
		AggregateID:      uuid.New(),
		AggregateVersion: 1,
		Producer:         "worker",
		Payload:          ObjectGCRequestedV1{ObjectID: uuid.New()},
	}
	if _, err := New(EnvelopeInput{EventType: "future.unknown.v1", Payload: map[string]any{}}); !errors.Is(err, ErrUnknownEventType) {
		t.Fatalf("unknown route error = %v", err)
	}
	base.MessageKind = MessageKindDomainEvent
	if _, err := New(base); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("mismatched kind error = %v", err)
	}
}

func TestEnvelopeAcceptsAdditivePayloadFields(t *testing.T) {
	data := []byte(`{"owner_id":"11111111-1111-4111-8111-111111111111","file_entry_id":"22222222-2222-4222-8222-222222222222","object_id":"33333333-3333-4333-8333-333333333333","future_field":"ignored"}`)
	if _, err := DecodePayload(EventFileCreatedV1, data); err != nil {
		t.Fatalf("additive payload rejected: %v", err)
	}
}
