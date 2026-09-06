package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventstream"
	"github.com/google/uuid"
)

// PublishKafkaRecord is the transport-specific sink boundary. It does not
// mutate delivery state; the relay owns that transaction and marks the Kafka
// delivery published only after this call succeeds.
func PublishKafkaRecord(ctx context.Context, producer eventstream.Producer, event Event) error {
	if producer == nil || event.ID == uuid.Nil || event.EventType == "" || len(event.Payload) == 0 {
		return errors.New("outbox: invalid Kafka event")
	}
	topic := eventstream.TopicForEvent(event.EventType)
	if topic == "" {
		return errors.New("outbox: event has no Kafka topic")
	}
	envelope, err := eventEnvelope(event)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if err := producer.Publish(ctx, eventstream.Record{Topic: topic, Key: event.AggregateID.String(), EventID: event.ID.String(), EventType: event.EventType, Payload: payload}); err != nil {
		return err
	}
	return nil
}

func eventEnvelope(event Event) (eventcontract.Envelope, error) {
	definition, ok := eventcontract.Lookup(event.EventType)
	if !ok {
		return eventcontract.Envelope{}, errors.New("outbox: event has unknown contract")
	}
	kind := definition.MessageKind
	producer := "legacy-outbox"
	if event.Producer != nil && *event.Producer != "" {
		producer = *event.Producer
	}
	occurred := event.CreatedAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	if event.OccurredAt != nil && !event.OccurredAt.IsZero() {
		occurred = *event.OccurredAt
	}
	var trace *eventcontract.TraceContext
	if len(event.TraceContext) != 0 {
		var carrier eventcontract.TraceContext
		if err := json.Unmarshal(event.TraceContext, &carrier); err == nil {
			trace = &carrier
		}
	}
	return eventcontract.Envelope{EventID: event.ID, EventType: event.EventType, MessageKind: kind, EventVersion: event.EventVersion, AggregateType: event.AggregateType, AggregateID: event.AggregateID, AggregateVersion: event.AggregateVersion, Producer: producer, OccurredAt: occurred, Trace: trace, Payload: event.Payload}, nil
}
