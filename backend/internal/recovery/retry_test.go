package recovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestMediaRecoveryBudgetBelongsToStorageGeneration(t *testing.T) {
	for _, terminal := range []string{"stopped", "dead"} {
		t.Run(terminal, func(t *testing.T) {
			db := recoveryDB(t)
			id := uuid.New()
			now := time.Now().UTC()
			born := now.Add(-time.Hour)
			object := objectstore.Object{ID: id, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(id.String()))), SizeBytes: 1, MimeType: "image/png", Bucket: "test", ObjectKey: "generation-one/" + id.String(), Status: "ready", Version: 1, VerifiedAt: &born}
			if err := db.Create(&object).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				db.Exec("DELETE FROM task_results WHERE split_part(task_id, ':', 1) IN (SELECT id::text FROM outbox_events WHERE aggregate_id = ?)", id)
				db.Exec("DELETE FROM outbox_events WHERE aggregate_id = ?", id)
				db.Delete(&objectstore.Object{}, "id = ?", id)
			})
			scan := func() Report {
				t.Helper()
				report := Report{ByKind: map[string]int{}}
				if err := New(db, false).WithClock(func() time.Time { return now }).recoverMedia(context.Background(), 100, &report); err != nil {
					t.Fatal(err)
				}
				return report
			}
			cycles := MaxRecoveryEvents
			if terminal == "dead" {
				cycles = 1
			}
			for range cycles {
				scan()
				var event outbox.Event
				if err := db.Where("aggregate_id = ? AND status = 'pending'", id).Take(&event).Error; err != nil {
					t.Fatal(err)
				}
				old := now.Add(-time.Hour)
				if err := db.Model(&event).Updates(map[string]any{"status": "published", "published_at": old}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&outbox.Delivery{}).Where("event_id = ?", event.ID).Updates(map[string]any{"status": "published", "published_at": old}).Error; err != nil {
					t.Fatal(err)
				}
				status := "succeeded"
				if terminal == "dead" {
					status = "dead"
				}
				if err := db.Create(&taskqueue.Result{TaskID: event.ID.String() + ":" + outbox.Routes(event.EventType, true)[0].Destination, Status: status, FinishedAt: old}).Error; err != nil {
					t.Fatal(err)
				}
			}
			scan()
			// Reference/version changes alone must not reset this budget.
			if err := db.Model(&object).Update("version", 2).Error; err != nil {
				t.Fatal(err)
			}
			scan()
			var count int64
			db.Model(&outbox.Event{}).Where("aggregate_id = ?", id).Count(&count)
			if count != int64(cycles) {
				t.Fatalf("same generation reset retry budget: %d", count)
			}
			// A real revival changes both storage key and generation birth time.
			reborn := time.Now().UTC()
			if err := db.Model(&object).Updates(map[string]any{"object_key": "generation-two/" + id.String(), "verified_at": reborn, "version": 4}).Error; err != nil {
				t.Fatal(err)
			}
			scan()
			db.Model(&outbox.Event{}).Where("aggregate_id = ?", id).Count(&count)
			if count != int64(cycles+1) {
				t.Fatalf("old terminal receipt blocked new generation: events=%d", count)
			}
		})
	}
}

func TestRecoveryIdentityBudgetPersistsAcrossScannersAndTerminalReceipts(t *testing.T) {
	for _, status := range []string{"succeeded", "dead"} {
		t.Run(status, func(t *testing.T) {
			db := recoveryDB(t)
			id := uuid.New()
			if _, err := storageops.Enqueue(db, storageops.Input{ID: id, OperationType: storageops.MultipartAbort, IdempotencyKey: uuid.NewString()}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			key := "bounded-recovery:" + id.String()
			input := eventcontract.EnvelopeInput{EventType: outbox.EventUploadOperationRecover, AggregateID: id, AggregateVersion: 1, Producer: "maintenance-recovery", Payload: eventcontract.UploadOperationRecoverV1{OperationID: id, OperationType: storageops.MultipartAbort, RecoveryKey: key}}
			t.Cleanup(func() {
				db.Exec("DELETE FROM task_results WHERE split_part(task_id, ':', 1) IN (SELECT id::text FROM outbox_events WHERE aggregate_id = ?)", id)
				db.Exec("DELETE FROM outbox_events WHERE aggregate_id = ?", id)
				db.Delete(&storageops.Operation{}, "id = ?", id)
			})
			for cycle := 0; cycle < MaxRecoveryEvents; cycle++ {
				r := New(db, false).WithClock(func() time.Time { return now })
				created, err := r.ensureStorageOperation(context.Background(), key, "storage_operation", id, 1, input)
				if err != nil || !created {
					t.Fatalf("cycle %d: created=%v err=%v", cycle, created, err)
				}
				var event outbox.Event
				if err := db.Where("aggregate_id = ? AND status = 'pending'", id).Take(&event).Error; err != nil {
					t.Fatal(err)
				}
				old := now.Add(-time.Hour)
				if err := db.Model(&outbox.Event{}).Where("id = ?", event.ID).Updates(map[string]any{"status": "published", "published_at": old}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&outbox.Delivery{}).Where("event_id = ?", event.ID).Updates(map[string]any{"status": "published", "published_at": old}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Create(&taskqueue.Result{TaskID: event.ID.String() + ":" + outbox.TaskUploadOperationRecoverV1 + "@upload", Status: status, FinishedAt: old}).Error; err != nil {
					t.Fatal(err)
				}
				now = now.Add(time.Hour)
			}
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					created, err := New(db, false).WithClock(func() time.Time { return now }).ensureStorageOperation(context.Background(), key, "storage_operation", id, 1, input)
					if err != nil || created {
						t.Errorf("exhausted recovery restarted: %v %v", created, err)
					}
				}()
			}
			wg.Wait()
			var count, stopped int64
			db.Model(&outbox.Event{}).Where("aggregate_id = ?", id).Count(&count)
			db.Model(&outbox.Recovery{}).Where("resource_id = ? AND stopped_at IS NOT NULL", id).Count(&stopped)
			if count != MaxRecoveryEvents || stopped != MaxRecoveryEvents {
				t.Fatalf("events=%d stopped=%d", count, stopped)
			}
			operation, err := storageops.Get(db, id)
			if err != nil || operation.State != storageops.StateDead {
				t.Fatalf("exhausted recovery left storage retryable: %+v %v", operation, err)
			}
		})
	}
}

func TestStorageRecoveryScanMovesPastActiveOldestRows(t *testing.T) {
	db := recoveryDB(t)
	var ids []uuid.UUID
	for range 5 {
		op, err := storageops.Enqueue(db, storageops.Input{OperationType: storageops.MultipartAbort, IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, op.ID)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM outbox_events WHERE aggregate_id IN ?", ids)
		db.Delete(&storageops.Operation{}, "id IN ?", ids)
	})
	r := New(db, false)
	for range len(ids) {
		report := Report{ByKind: map[string]int{}}
		if err := r.recoverStorageOperations(context.Background(), 1, &report); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Model(&outbox.Event{}).Where("aggregate_id IN ?", ids).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != int64(len(ids)) {
		t.Fatalf("only %d of %d operations reached; active rows starved the scan", count, len(ids))
	}
}

func TestStorageRecoveryFailureDoesNotBlockLaterRows(t *testing.T) {
	db := recoveryDB(t)
	var ids []uuid.UUID
	for range 2 {
		op, err := storageops.Enqueue(db, storageops.Input{OperationType: storageops.MultipartAbort, IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, op.ID)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM outbox_events WHERE aggregate_id IN ?", ids)
		db.Delete(&storageops.Operation{}, "id IN ?", ids)
	})
	const callback = "test:broken_recovery_event"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if event, ok := tx.Statement.Dest.(*outbox.Event); ok && event.AggregateID == ids[0] {
			tx.AddError(errors.New("injected bad event"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
	report := Report{ByKind: map[string]int{}}
	if err := New(db, false).recoverStorageOperations(context.Background(), 10, &report); err == nil {
		t.Fatal("broken event was hidden")
	}
	var n int64
	if err := db.Model(&outbox.Event{}).Where("aggregate_id = ?", ids[1]).Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("healthy recovery starved: count=%d err=%v", n, err)
	}
}
