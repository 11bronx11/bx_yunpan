package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventstream"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type kafkaProbeProducer struct{ records []eventstream.Record }

func (p *kafkaProbeProducer) Publish(_ context.Context, record eventstream.Record) error {
	p.records = append(p.records, record)
	return nil
}
func (*kafkaProbeProducer) Health(context.Context) error { return nil }

type kafkaFlakyProducer struct {
	records []eventstream.Record
	fail    bool
}

func (p *kafkaFlakyProducer) Publish(_ context.Context, record eventstream.Record) error {
	if p.fail {
		return errors.New("local kafka broker unavailable")
	}
	p.records = append(p.records, record)
	return nil
}

func (*kafkaFlakyProducer) Health(context.Context) error { return nil }

// This opt-in probe exercises the actual PostgreSQL delivery state machine.
// It does not contact a Kafka broker; broker fault/replay remains a staging
// gate because no broker is part of the development Compose profile.
func TestKafkaDispatcherPublishesAndMarksDelivery(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ownerID, fileID, objectID := uuid.New(), uuid.New(), uuid.New()
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventType: eventcontract.EventFileUpdatedV1, AggregateID: fileID, AggregateVersion: 2, Producer: "kafka-probe",
		Payload: eventcontract.FileUpdatedV1{OwnerID: ownerID, FileEntryID: fileID, ObjectID: objectID, OldName: "a", NewName: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithKafkaEnabled(WithV2EventsEnabled(context.Background(), true), true)
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return AddEnvelopeContext(ctx, tx, envelope) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&Event{}, "id = ?", envelope.EventID).Error })

	producer := &kafkaProbeProducer{}
	if err := NewKafkaDispatcher(db, producer, nil).DispatchBatch(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if len(producer.records) != 1 || producer.records[0].Topic != "yunpan.file-events" {
		t.Fatalf("published records = %#v", producer.records)
	}
	var payload map[string]any
	if err := json.Unmarshal(producer.records[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["event_type"] != eventcontract.EventFileUpdatedV1 || payload["event_id"] != envelope.EventID.String() {
		t.Fatalf("published envelope = %#v", payload)
	}
	var delivery Delivery
	if err := db.Where("event_id = ? AND transport = 'kafka'", envelope.EventID).First(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "published" {
		t.Fatalf("delivery status = %q", delivery.Status)
	}
}

// This local staging probe models a broker outage at the producer boundary.
// PostgreSQL delivery state must remain retryable, and a later successful
// publish must advance the same delivery instead of creating a second event.
func TestKafkaDispatcherRetriesAfterProducerRecovery(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ownerID, fileID, objectID := uuid.New(), uuid.New(), uuid.New()
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventType: eventcontract.EventFileUpdatedV1, AggregateID: fileID, AggregateVersion: 3, Producer: "kafka-recovery-probe",
		Payload: eventcontract.FileUpdatedV1{OwnerID: ownerID, FileEntryID: fileID, ObjectID: objectID, OldName: "before", NewName: "after"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithKafkaEnabled(WithV2EventsEnabled(context.Background(), true), true)
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return AddEnvelopeContext(ctx, tx, envelope) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&Event{}, "id = ?", envelope.EventID).Error })

	producer := &kafkaFlakyProducer{fail: true}
	dispatcher := NewKafkaDispatcher(db, producer, nil)
	if err := dispatcher.DispatchBatch(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var delivery Delivery
	if err := db.Where("event_id = ? AND transport = 'kafka'", envelope.EventID).First(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "failed" || delivery.Attempt != 1 || delivery.LastError == nil {
		t.Fatalf("failed delivery = %#v", delivery)
	}
	if err := db.Model(&Delivery{}).Where("event_id = ? AND transport = 'kafka'", envelope.EventID).Updates(map[string]any{"available_at": time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}

	producer.fail = false
	if err := dispatcher.DispatchBatch(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("event_id = ? AND transport = 'kafka'", envelope.EventID).First(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "published" || delivery.Attempt != 2 || len(producer.records) != 1 {
		t.Fatalf("recovered delivery = %#v, records = %d", delivery, len(producer.records))
	}
	var event Event
	if err := db.First(&event, "id = ?", envelope.EventID).Error; err != nil {
		t.Fatal(err)
	}
	if event.Status != "published" {
		t.Fatalf("event status after recovery = %q", event.Status)
	}
}
