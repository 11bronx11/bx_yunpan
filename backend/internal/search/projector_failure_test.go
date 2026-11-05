package search

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type flakyProjectionIndex struct {
	mu             sync.Mutex
	failures       int
	tombstoneCalls int
}

type concurrentProjectionIndex struct {
	mu    sync.Mutex
	calls []struct {
		fileID  uuid.UUID
		version int64
	}
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *concurrentProjectionIndex) UpsertContent(context.Context, ContentProjection) error {
	return nil
}
func (f *concurrentProjectionIndex) UpsertFile(context.Context, FileProjection) error { return nil }
func (f *concurrentProjectionIndex) Health(context.Context) error                     { return nil }
func (f *concurrentProjectionIndex) TombstoneObjectContent(context.Context, string, uuid.UUID, int64) error {
	return nil
}
func (f *concurrentProjectionIndex) TombstoneFile(_ context.Context, _ string, fileID uuid.UUID, version int64) error {
	f.once.Do(func() { close(f.entered) })
	<-f.release
	f.mu.Lock()
	f.calls = append(f.calls, struct {
		fileID  uuid.UUID
		version int64
	}{fileID: fileID, version: version})
	f.mu.Unlock()
	return nil
}

func (f *flakyProjectionIndex) UpsertContent(context.Context, ContentProjection) error { return nil }
func (f *flakyProjectionIndex) UpsertFile(context.Context, FileProjection) error       { return nil }
func (f *flakyProjectionIndex) Health(context.Context) error                           { return nil }
func (f *flakyProjectionIndex) TombstoneObjectContent(context.Context, string, uuid.UUID, int64) error {
	return nil
}
func (f *flakyProjectionIndex) TombstoneFile(context.Context, string, uuid.UUID, int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tombstoneCalls++
	if f.failures > 0 {
		f.failures--
		return errors.New("injected external timeout")
	}
	return nil
}

func TestProjectorExternalCheckpointRecoversAfterPublishFailure(t *testing.T) {
	dsn := testPostgresDSN(t)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	payload := eventcontract.FileDeletedV1{OwnerID: uuid.New(), FileEntryID: uuid.New(), ObjectID: uuid.New(), TombstoneAt: time.Now().UTC()}
	raw, _ := json.Marshal(payload)
	event := outbox.Event{ID: eventID, AggregateType: "file_entry", AggregateID: payload.FileEntryID, EventType: eventcontract.EventFileDeletedV1, EventVersion: 1, Payload: raw, Status: "pending", AvailableAt: time.Now().UTC(), CreatedAt: time.Now().UTC()}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = db.Where("event_id = ?", eventID).Delete(&outbox.Consumption{}).Error
		_ = db.Where("event_id = ?", eventID).Delete(&ProjectionCheckpoint{}).Error
		_ = db.Where("id = ?", eventID).Delete(&outbox.Event{}).Error
	}()
	value := map[string]any{"event_id": eventID, "event_type": eventcontract.EventFileDeletedV1, "event_version": 1, "aggregate_version": int64(4)}
	for key, field := range map[string]any{"owner_id": payload.OwnerID, "file_entry_id": payload.FileEntryID, "object_id": payload.ObjectID, "tombstone_at": payload.TombstoneAt} {
		value[key] = field
	}
	encoded, _ := json.Marshal(value)
	fake := &flakyProjectionIndex{failures: 1}
	projector := NewProjector(db, "v-test").WithExternalIndex(fake)
	task := taskqueue.NewTask(outbox.TaskSearchProjectV1, encoded)
	if err := projector.Handler(context.Background(), task); err == nil {
		t.Fatal("first publish should fail")
	}
	checkpoint, err := projector.Checkpoint(context.Background(), eventID)
	if err != nil || checkpoint.State != "failed" || checkpoint.Attempt != 1 || checkpoint.LastError == nil {
		t.Fatalf("failed checkpoint = %#v, err=%v", checkpoint, err)
	}
	if err := projector.Handler(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = projector.Checkpoint(context.Background(), eventID)
	if err != nil || checkpoint.State != "tombstoned" || checkpoint.Attempt != 2 || checkpoint.Checksum == nil || checkpoint.Watermark == nil || *checkpoint.Watermark != 4 {
		t.Fatalf("recovered checkpoint = %#v, err=%v", checkpoint, err)
	}
	var count int64
	if err := db.Model(&outbox.Consumption{}).Where("event_id = ? AND consumer_name = ?", eventID, projector.consumerName()).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("consumption count = %d, err=%v", count, err)
	}
	fake.mu.Lock()
	calls := fake.tombstoneCalls
	fake.mu.Unlock()
	if calls != 2 {
		t.Fatalf("external calls = %d, want retry", calls)
	}
}

func TestProjectorCheckpointIndexedCrashWindowOnlyAcknowledges(t *testing.T) {
	dsn := testPostgresDSN(t)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	payload := eventcontract.FileDeletedV1{OwnerID: uuid.New(), FileEntryID: uuid.New(), ObjectID: uuid.New(), TombstoneAt: time.Now().UTC()}
	raw, _ := json.Marshal(payload)
	if err := db.Create(&outbox.Event{ID: eventID, AggregateType: "file_entry", AggregateID: payload.FileEntryID, EventType: eventcontract.EventFileDeletedV1, EventVersion: 1, Payload: raw, Status: "pending", AvailableAt: time.Now().UTC(), CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = db.Where("event_id = ?", eventID).Delete(&outbox.Consumption{}).Error
		_ = db.Where("event_id = ?", eventID).Delete(&ProjectionCheckpoint{}).Error
		_ = db.Where("id = ?", eventID).Delete(&outbox.Event{}).Error
	}()
	if err := db.Create(&ProjectionCheckpoint{EventID: eventID, ConsumerName: NewProjector(db, "v-test").WithExternalIndex(&flakyProjectionIndex{}).consumerName(), State: "indexed", Attempt: 1, UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	fake := &flakyProjectionIndex{}
	projector := NewProjector(db, "v-test").WithExternalIndex(fake)
	value := map[string]any{"event_id": eventID, "event_type": eventcontract.EventFileDeletedV1, "event_version": 1, "file_entry_id": payload.FileEntryID, "owner_id": payload.OwnerID, "object_id": payload.ObjectID, "tombstone_at": payload.TombstoneAt}
	encoded, _ := json.Marshal(value)
	if err := projector.Handler(context.Background(), taskqueue.NewTask(outbox.TaskSearchProjectV1, encoded)); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	calls := fake.tombstoneCalls
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("crash-window replay called external %d times", calls)
	}
}

func TestProjectorConcurrentDuplicateDeliveryIsIdempotent(t *testing.T) {
	dsn := testPostgresDSN(t)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	fileID := uuid.New()
	payload := eventcontract.FileDeletedV1{OwnerID: uuid.New(), FileEntryID: fileID, ObjectID: uuid.New(), TombstoneAt: time.Now().UTC()}
	raw, _ := json.Marshal(payload)
	if err := db.Create(&outbox.Event{ID: eventID, AggregateType: "file_entry", AggregateID: fileID, EventType: eventcontract.EventFileDeletedV1, EventVersion: 1, Payload: raw, Status: "pending", AvailableAt: time.Now().UTC(), CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = db.Where("event_id = ?", eventID).Delete(&outbox.Consumption{}).Error
		_ = db.Where("event_id = ?", eventID).Delete(&ProjectionCheckpoint{}).Error
		_ = db.Where("id = ?", eventID).Delete(&outbox.Event{}).Error
	}()

	value := map[string]any{"event_id": eventID, "event_type": eventcontract.EventFileDeletedV1, "event_version": 1, "aggregate_version": int64(7), "file_entry_id": fileID, "owner_id": payload.OwnerID, "object_id": payload.ObjectID, "tombstone_at": payload.TombstoneAt}
	encoded, _ := json.Marshal(value)
	fake := &concurrentProjectionIndex{entered: make(chan struct{}), release: make(chan struct{})}
	projector := NewProjector(db, "v-concurrent").WithExternalIndex(fake)
	task := taskqueue.NewTask(outbox.TaskSearchProjectV1, encoded)
	results := make(chan error, 2)
	go func() { results <- projector.Handler(context.Background(), task) }()
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first external publish did not start")
	}
	go func() { results <- projector.Handler(context.Background(), task) }()
	time.Sleep(50 * time.Millisecond)
	close(fake.release)
	if first, second := <-results, <-results; first != nil || second != nil {
		t.Fatalf("concurrent handlers = %v, %v", first, second)
	}
	var count int64
	if err := db.Model(&outbox.Consumption{}).Where("event_id = ? AND consumer_name = ?", eventID, projector.consumerName()).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("completion marker count = %d, err=%v", count, err)
	}
	checkpoint, err := projector.Checkpoint(context.Background(), eventID)
	if err != nil || checkpoint.State != "tombstoned" {
		t.Fatalf("checkpoint = %#v, err=%v", checkpoint, err)
	}
	fake.mu.Lock()
	calls := append([]struct {
		fileID  uuid.UUID
		version int64
	}{}, fake.calls...)
	fake.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("expected at least one external publish")
	}
	for _, call := range calls {
		if call.fileID != fileID || call.version != 7 {
			t.Fatalf("unstable external identity: %#v", call)
		}
	}
}

func testPostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	return dsn
}
