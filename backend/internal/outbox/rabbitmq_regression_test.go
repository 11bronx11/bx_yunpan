package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"log/slog"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
)

func TestPayloadPreservesReprocessBusinessTaskID(t *testing.T) {
	id := uuid.New()
	e := Event{ID: uuid.New(), EventType: EventAIReprocessRequested, Payload: []byte(`{"task_id":"` + id.String() + `"}`)}
	var got map[string]any
	if err := json.Unmarshal(payloadWithTrace(e, nil), &got); err != nil {
		t.Fatal(err)
	}
	if got["task_id"] != id.String() || got["event_id"] != e.ID.String() {
		t.Fatalf("identity changed: %v", got)
	}
}
func TestRabbitGCDelaySurvivesRestartAndConfigurationChange(t *testing.T) {
	db, id := newDeliveryFixture(t)
	if err := db.Model(&Delivery{}).Where("event_id = ?", id).Update("destination", TaskObjectGCV1+"@maintenance").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Event{}).Where("id = ?", id).Updates(map[string]any{"event_type": EventObjectGCRequested, "payload": []byte(`{"object_id":"` + uuid.NewString() + `"}`)}).Error; err != nil {
		t.Fatal(err)
	}
	d := newDispatcher(db, &recordingPublisher{}, slog.Default(), config.Outbox{BatchSize: 10, GCDelay: time.Hour})
	if err := d.prepareDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	var row Delivery
	if err := db.First(&row, "event_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if row.NotBefore == nil || row.NotBefore.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("delay missing: %v", row.NotBefore)
	}
	pinned := *row.NotBefore
	d = newDispatcher(db, &recordingPublisher{}, slog.Default(), config.Outbox{BatchSize: 10, GCDelay: 0})
	if err := d.prepareDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, err := d.claimDeliveries(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("premature claim: %v %v", items, err)
	}
	if err := db.First(&row, "event_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if !row.NotBefore.Equal(pinned) {
		t.Fatal("restart changed delay")
	}
	if err := db.Model(&Delivery{}).Where("event_id = ?", id).Update("not_before", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if items, err = d.claimDeliveries(context.Background()); err != nil || len(items) != 1 {
		t.Fatalf("due claim: %d %v", len(items), err)
	}
}

type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, taskqueue.Task) error {
	return errors.New("broker down")
}
func TestRabbitPublishFailureLeavesDurableRetry(t *testing.T) {
	db, id := newDeliveryFixture(t)
	d := newDispatcher(db, failingPublisher{}, slog.Default(), config.Outbox{BatchSize: 1})
	if err := d.dispatchBatch(context.Background()); err == nil {
		t.Fatal("publish error hidden")
	}
	var row Delivery
	if e := db.First(&row, "event_id = ?", id).Error; e != nil {
		t.Fatal(e)
	}
	if row.Status != "failed" || row.LastError == nil || row.LeaseToken != nil || !row.AvailableAt.After(time.Now()) {
		t.Fatalf("retry state: %+v", row)
	}
}
func TestRabbitPreparationPreservesLegacyPublishedOwnership(t *testing.T) {
	db, id := newDeliveryFixture(t)
	if err := db.Model(&Delivery{}).Where("event_id = ?", id).Updates(map[string]any{"transport": "asynq", "destination": EventObjectVerifyRequested + "@object", "status": "published"}).Error; err != nil {
		t.Fatal(err)
	}
	d := newDispatcher(db, &recordingPublisher{}, slog.Default(), config.Outbox{BatchSize: 1})
	if err := d.prepareDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&Delivery{}).Where("event_id = ?", id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("created second transport owner: count=%d", count)
	}
}

func TestLegacyImportPreservesBudgetAndDeadlineIdempotently(t *testing.T) {
	db, id := newDeliveryFixture(t)
	if err := db.Model(&Delivery{}).Where("event_id = ?", id).Updates(map[string]any{"transport": "asynq", "status": "published"}).Error; err != nil {
		t.Fatal(err)
	}
	task := taskqueue.Task{ID: id.String() + ":" + TaskObjectVerifyV1 + "@object", Kind: TaskObjectVerifyV1, Queue: "object", Body: []byte(`{}`), RetryCount: 5, MaxRetry: 8, Timeout: 30 * time.Minute}
	due := time.Now().UTC().Add(time.Hour)
	for i := 0; i < 2; i++ {
		if err := db.Transaction(func(tx *gorm.DB) error { return StageLegacyTask(tx, task, due, 24*time.Hour, true) }); err != nil {
			t.Fatal(err)
		}
	}
	var d Delivery
	if err := db.First(&d, "event_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if d.Transport != "rabbitmq" || d.Status != "pending" || d.TaskRetryCount != 5 || d.TaskMaxRetry == nil || *d.TaskMaxRetry != 8 || d.TaskTimeoutMS == nil || *d.TaskTimeoutMS != task.Timeout.Milliseconds() || d.NotBefore == nil || d.NotBefore.Before(due.Add(-time.Millisecond)) {
		t.Fatalf("legacy scheduling lost: %+v", d)
	}
	var count int64
	if err := db.Table("legacy_task_imports").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("duplicate import rows=%d error=%v", count, err)
	}
}
