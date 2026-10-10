package recovery

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
)

func recoveryDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRecoveryReplaysMissingTaskWithStableIdentityAndKeepsTerminal(t *testing.T) {
	db := recoveryDB(t)
	id := uuid.New()
	key := "repeat-loss:" + id.String()
	input := eventcontract.EnvelopeInput{
		EventType: eventcontract.EventStorageOperationRecoverV1, AggregateID: id, AggregateVersion: 1,
		Producer: "maintenance-recovery", Payload: eventcontract.StorageOperationRecoverV1{OperationID: id, OperationType: "object.delete", RecoveryKey: key},
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM outbox_recoveries WHERE resource_id = ?", id).Error
		_ = db.Exec("DELETE FROM outbox_events WHERE aggregate_id = ?", id).Error
	})
	now := time.Now().UTC()
	r := New(db, false).WithStaleAfter(time.Minute).WithClock(func() time.Time { return now })
	ensure := func(want bool) {
		t.Helper()
		created, err := r.ensure(context.Background(), key, "broker_task_missing", "storage_operation", id, 1, input)
		if err != nil || created != want {
			t.Fatalf("created=%v want=%v error=%v", created, want, err)
		}
	}
	ensure(true)
	for i := 0; i < 3; i++ {
		ensure(false)
		if err := db.Exec("UPDATE outbox_deliveries SET status = 'sending' WHERE event_id IN (SELECT id FROM outbox_events WHERE aggregate_id = ?) AND status = 'pending'", id).Error; err != nil {
			t.Fatal(err)
		}
		ensure(false)
		if err := db.Exec("UPDATE outbox_deliveries SET status = 'published', published_at = ? WHERE event_id IN (SELECT id FROM outbox_events WHERE aggregate_id = ?)", now, id).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("UPDATE outbox_events SET status = 'published', published_at = ? WHERE aggregate_id = ? AND status = 'pending'", now, id).Error; err != nil {
			t.Fatal(err)
		}
		ensure(false)
		now = now.Add(2 * time.Hour)
		ensure(true)
	}
	var published outbox.Event
	if err := db.Where("aggregate_id = ?", id).First(&published).Error; err != nil {
		t.Fatal(err)
	}
	taskID := published.ID.String() + ":" + outbox.EventStorageOperationRecover + "@maintenance"
	if err := db.Exec("INSERT INTO task_results(task_id,status,finished_at) VALUES(?,'dead',now())", taskID).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM task_results WHERE task_id = ?", taskID) })
	if err := db.Exec("UPDATE outbox_deliveries SET status='published' WHERE event_id=?", published.ID).Error; err != nil {
		t.Fatal(err)
	}
	ensure(false)
	var count int64
	if err := db.Model(&outbox.Event{}).Where("aggregate_id = ?", id).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("recovery events=%d error=%v", count, err)
	}
}

func TestRunOnceRebuildsEveryPersistentWorkClassIdempotently(t *testing.T) {
	db := recoveryDB(t)
	userID, folderID, objectID := uuid.New(), uuid.New(), uuid.New()
	sessionID, taskID, operationID := uuid.New(), uuid.New(), uuid.New()
	username := "recovery-" + userID.String()
	if err := db.Exec(`INSERT INTO users (id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'test')`, userID, username, username).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO folders (id, owner_id, name, name_normalized) VALUES (?, ?, '/', '/')`, folderID, userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO file_objects (id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count, version) VALUES (?, repeat('a', 64), 10, 'image/jpeg', 'test', ?, 'ready', 0, 2)`, objectID, "objects/recovery-"+objectID.String()).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO upload_sessions (id, user_id, folder_id, filename, filename_normalized, declared_sha256, mime_type, size_bytes, reserved_bytes, bucket, object_key, storage_upload_id, part_size, part_count, status, idempotency_key, expires_at, version) VALUES (?, ?, ?, 'recover.txt', 'recover.txt', repeat('b', 64), 'text/plain', 10, 10, 'test', ?, 'upload-id', 10, 1, 'verifying', ?, now() + interval '1 hour', 3)`, sessionID, userID, folderID, "staging/"+sessionID.String(), "idem-"+sessionID.String()).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO async_tasks (id, owner_id, task_type, dedupe_key, resource_type, resource_id, status, updated_at, version) VALUES (?, ?, 'ai.reprocess', ?, 'file', ?, 'pending', now() - interval '1 hour', 2)`, taskID, userID, "recover-"+taskID.String(), objectID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO storage_operations (id, operation_type, idempotency_key, object_id, source_bucket, source_key, state, available_at) VALUES (?, 'object.delete', ?, ?, 'test', 'objects/recovery', 'pending', now())`, operationID, "recover-op-"+operationID.String(), objectID).Error; err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = db.Exec(`DELETE FROM outbox_recoveries WHERE resource_id IN (?, ?, ?) OR recovery_key LIKE ?`, sessionID, objectID, taskID, "%"+userID.String()+"%").Error
		_ = db.Exec(`DELETE FROM outbox_events WHERE producer = 'maintenance-recovery' AND id NOT IN (SELECT event_id FROM outbox_recoveries)`).Error
		_ = db.Exec(`DELETE FROM storage_operations WHERE id = ?`, operationID).Error
		_ = db.Exec(`DELETE FROM async_tasks WHERE id = ?`, taskID).Error
		_ = db.Exec(`DELETE FROM upload_sessions WHERE id = ?`, sessionID).Error
		_ = db.Exec(`DELETE FROM file_objects WHERE id = ?`, objectID).Error
		_ = db.Exec(`DELETE FROM folders WHERE id = ?`, folderID).Error
		_ = db.Exec(`DELETE FROM users WHERE id = ?`, userID).Error
	}
	t.Cleanup(cleanup)

	reconciler := New(db, true).WithStaleAfter(time.Second).WithOperationStaleAfter(time.Second)
	first, err := reconciler.RunOnce(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if first.Repaired < 5 {
		t.Fatalf("repaired = %d, report = %#v", first.Repaired, first)
	}
	var recoveryCount, deliveryCount int64
	if err := db.Table("outbox_recoveries").Where("resource_id IN (?, ?, ?)", sessionID, objectID, taskID).Count(&recoveryCount).Error; err != nil {
		t.Fatal(err)
	}
	if recoveryCount < 5 {
		t.Fatalf("recovery rows = %d", recoveryCount)
	}
	if err := db.Table("outbox_deliveries d").Joins("JOIN outbox_events e ON e.id = d.event_id").Where("e.producer = 'maintenance-recovery'").Count(&deliveryCount).Error; err != nil {
		t.Fatal(err)
	}
	if deliveryCount < recoveryCount {
		t.Fatalf("delivery rows = %d, recovery rows = %d", deliveryCount, recoveryCount)
	}
	second, err := reconciler.RunOnce(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if second.Repaired != 0 {
		t.Fatalf("second scan repaired = %d, report = %#v", second.Repaired, second)
	}
}

func TestRecoverStorageOperationsHonorsAvailabilityAndStaleLeases(t *testing.T) {
	db := recoveryDB(t)
	now := time.Now().UTC()
	ids := map[string]uuid.UUID{
		"future-pending":  uuid.New(),
		"ready-pending":   uuid.New(),
		"ready-failed":    uuid.New(),
		"active-running":  uuid.New(),
		"stale-running":   uuid.New(),
		"expired-running": uuid.New(),
	}
	for name, id := range ids {
		state := "pending"
		availableAt := now.Add(-time.Minute)
		updatedAt := now
		var leaseUntil any
		switch name {
		case "future-pending":
			availableAt = now.Add(time.Hour)
		case "ready-failed":
			state = "failed"
		case "active-running":
			state = "running"
			updatedAt = now.Add(-2 * time.Hour)
			leaseUntil = now.Add(time.Hour)
		case "stale-running":
			state = "running"
			updatedAt = now.Add(-2 * time.Hour)
		case "expired-running":
			state = "running"
			leaseUntil = now.Add(-time.Minute)
		}
		if err := db.Exec(`
			INSERT INTO storage_operations
				(id, operation_type, idempotency_key, state, available_at, lease_until, updated_at)
			VALUES (?, 'object.delete', ?, ?, ?, ?, ?)`,
			id, "recovery-boundary-"+name+"-"+id.String(), state, availableAt, leaseUntil, updatedAt).Error; err != nil {
			t.Fatal(err)
		}
	}

	cleanup := func() {
		for _, id := range ids {
			key := "storage.operation:" + id.String() + ":object.delete"
			_ = db.Exec(`DELETE FROM outbox_recoveries WHERE recovery_key = ?`, key).Error
			eventID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("bx-yunpan/recovery/"+key))
			_ = db.Exec(`DELETE FROM outbox_events WHERE id = ?`, eventID).Error
			_ = db.Exec(`DELETE FROM storage_operations WHERE id = ?`, id).Error
		}
	}
	t.Cleanup(cleanup)

	reconciler := New(db, false).WithOperationStaleAfter(time.Hour).WithClock(func() time.Time { return now })
	report := Report{ByKind: map[string]int{}}
	if err := reconciler.recoverStorageOperations(context.Background(), 20, &report); err != nil {
		t.Fatal(err)
	}
	if report.Repaired != 4 {
		t.Fatalf("repaired = %d, report = %#v", report.Repaired, report)
	}
	for _, name := range []string{"future-pending", "active-running"} {
		id := ids[name]
		var count int64
		if err := db.Table("outbox_recoveries").Where("recovery_key = ?", "storage.operation:"+id.String()+":object.delete").Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s was recovered unexpectedly", name)
		}
	}
}
