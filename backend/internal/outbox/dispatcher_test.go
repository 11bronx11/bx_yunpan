package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

type conflictAfterFirstEnqueue struct {
	calls   int
	taskIDs []string
}

func (f *conflictAfterFirstEnqueue) EnqueueContext(_ context.Context, _ *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	f.calls++
	info := &asynq.TaskInfo{}
	for _, option := range opts {
		if option.Type() == asynq.TaskIDOpt {
			info.ID, _ = option.Value().(string)
		}
	}
	f.taskIDs = append(f.taskIDs, info.ID)
	if f.calls > 1 {
		return nil, asynq.ErrTaskIDConflict
	}
	return info, nil
}

func TestFanoutRespectsAIEnabled(t *testing.T) {
	enabled := fanout(EventObjectReady, true)
	if len(enabled) != 2 || enabled[0].queue != "media" || enabled[1].queue != "ai" {
		t.Fatalf("enabled fanout = %#v", enabled)
	}

	disabled := fanout(EventObjectReady, false)
	if len(disabled) != 1 || disabled[0].queue != "media" {
		t.Fatalf("disabled fanout = %#v", disabled)
	}

	if targets := fanout(EventAIReprocessRequested, false); len(targets) != 0 {
		t.Fatalf("disabled reprocess fanout = %#v", targets)
	}
}

func TestPayloadWithTraceCarriesStableTaskMetadata(t *testing.T) {
	version := int64(7)
	event := Event{
		ID: uuid.New(), EventType: EventObjectReady, EventVersion: 1,
		AggregateVersion: &version, Payload: []byte(`{"object_id":"00000000-0000-0000-0000-000000000001"}`),
		CreatedAt: time.Now().UTC(),
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadWithTrace(event, nil), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["event_id"] != event.ID.String() || payload["task_id"] != event.ID.String() || payload["event_type"] != EventObjectReady || payload["aggregate_version"] != float64(7) {
		t.Fatalf("task metadata = %#v", payload)
	}
}

func TestDispatcherRecoversWhenEnqueueCommitsBeforeDeliveryStatus(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eventID, aggregateID := uuid.New(), uuid.New()
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventID: eventID, EventType: eventcontract.EventObjectVerifyRequestedV1,
		AggregateID: aggregateID, AggregateVersion: 1, Producer: "relay-window-test",
		Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: aggregateID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return AddEnvelope(tx, envelope) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&Event{}, "id = ?", eventID).Error })

	fake := &conflictAfterFirstEnqueue{}
	dispatcher := newDispatcher(db, fake, slog.Default(), config.Outbox{BatchSize: 1, MaxRetry: 2, TaskTimeout: time.Minute})
	injected := errors.New("injected delivery status failure")
	dispatcher.markPublished = func(*gorm.DB, Delivery, time.Time) error { return injected }
	if err := dispatcher.dispatchBatch(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("first dispatch error = %v", err)
	}
	var delivery Delivery
	if err := db.First(&delivery, "event_id = ?", eventID).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "pending" {
		t.Fatalf("rolled-back delivery status = %q", delivery.Status)
	}

	dispatcher.markPublished = markDeliveryPublished
	if err := dispatcher.dispatchBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 || len(fake.taskIDs) != 2 || fake.taskIDs[0] == "" || fake.taskIDs[0] != fake.taskIDs[1] {
		t.Fatalf("enqueue calls=%d task_ids=%v", fake.calls, fake.taskIDs)
	}
	if err := db.First(&delivery, "event_id = ?", eventID).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "published" {
		t.Fatalf("recovered delivery status = %q", delivery.Status)
	}
}
