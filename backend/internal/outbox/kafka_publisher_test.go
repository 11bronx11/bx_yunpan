package outbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventstream"
	"github.com/google/uuid"
)

type recordingProducer struct{ records []eventstream.Record }

func (p *recordingProducer) Publish(_ context.Context, record eventstream.Record) error {
	p.records = append(p.records, record)
	return nil
}
func (*recordingProducer) Health(context.Context) error { return nil }

func TestPublishKafkaRecordMapsAggregateAndTopic(t *testing.T) {
	producer := &recordingProducer{}
	ownerID, fileID, objectID := uuid.New(), uuid.New(), uuid.New()
	payload, err := json.Marshal(eventcontract.FileCreatedV1{OwnerID: ownerID, FileEntryID: fileID, ObjectID: objectID})
	if err != nil {
		t.Fatal(err)
	}
	event := Event{ID: uuid.New(), AggregateID: fileID, EventType: EventFileCreated, EventVersion: 1, AggregateType: "file_entry", Payload: payload}
	if err := PublishKafkaRecord(context.Background(), producer, event); err != nil {
		t.Fatal(err)
	}
	if len(producer.records) != 1 || producer.records[0].Topic != "yunpan.file-events" || producer.records[0].Key != event.AggregateID.String() {
		t.Fatalf("records = %#v", producer.records)
	}
	var envelope map[string]any
	if err := json.Unmarshal(producer.records[0].Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["event_id"] != event.ID.String() || envelope["event_type"] != EventFileCreated {
		t.Fatalf("Kafka payload is not a contract envelope: %#v", envelope)
	}
}
