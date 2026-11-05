package storageops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDurableRetryBudgetAndBackoffSurviveNewMessages(t *testing.T) {
	db := testDB(t)
	op, err := Enqueue(db, Input{OperationType: StagingDelete, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Delete(&Operation{}, "id = ?", op.ID) })
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		started, token, err := TryStartWithToken(db, op.ID, time.Minute)
		if err != nil || !started {
			t.Fatalf("attempt %d: started=%v err=%v", attempt, started, err)
		}
		err = MarkFailedOwned(db, op.ID, token, errors.New("provider unavailable"), time.Second)
		if attempt < MaxAttempts && err != nil {
			t.Fatal(err)
		}
		if (attempt == MaxAttempts) != errors.Is(err, ErrExhausted) {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		current, err := Get(db, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Attempt != attempt {
			t.Fatal(current.Attempt)
		}
		if attempt == 2 && current.AvailableAt.Sub(current.UpdatedAt) < 5*time.Second {
			t.Fatal("backoff did not increase")
		}
		if attempt < MaxAttempts {
			if started, _, err := TryStartWithToken(db, op.ID, time.Minute); err != nil || started {
				t.Fatalf("ignored backoff: %v %v", started, err)
			}
			if err := db.Model(&Operation{}).Where("id = ?", op.ID).Update("available_at", time.Now().Add(-time.Second)).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	current, _ := Get(db, op.ID)
	if current.State != StateDead || current.CompletedAt == nil || current.LastError == nil {
		t.Fatalf("missing terminal audit: %+v", current)
	}
	for range 3 {
		if started, _, err := TryStartWithToken(db, op.ID, time.Minute); err != nil || started {
			t.Fatalf("dead operation restarted: %v %v", started, err)
		}
	}
}

func TestFinalCrashedAttemptStopsOnlyAfterLeaseExpires(t *testing.T) {
	db := testDB(t)
	op, err := Enqueue(db, Input{OperationType: MultipartAbort, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Delete(&Operation{}, "id = ?", op.ID) })
	if err := db.Model(&Operation{}).Where("id = ?", op.ID).Update("attempt", MaxAttempts-1).Error; err != nil {
		t.Fatal(err)
	}
	started, _, err := TryStartWithToken(db, op.ID, time.Minute)
	if err != nil || !started {
		t.Fatal(started, err)
	}
	if err := StopExhausted(context.Background(), db, 10); err != nil {
		t.Fatal(err)
	}
	current, _ := Get(db, op.ID)
	if current.State != StateRunning {
		t.Fatal("stopped live final attempt")
	}
	if err := db.Model(&Operation{}).Where("id = ?", op.ID).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := StopExhausted(context.Background(), db, 10); err != nil {
		t.Fatal(err)
	}
	current, _ = Get(db, op.ID)
	if current.State != StateDead {
		t.Fatalf("crashed operation not stopped: %+v", current)
	}
}

func TestStorageLeaseRenewsAcrossRecoveryIdentities(t *testing.T) {
	db := testDB(t)
	op, err := Enqueue(db, Input{OperationType: CanonicalEnsure, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Delete(&Operation{}, "id = ?", op.ID) })
	lease := 900 * time.Millisecond
	started, token, err := TryStartWithToken(db, op.ID, lease)
	if err != nil || !started {
		t.Fatal(started, err)
	}
	op.LeaseToken = &token
	err = RunOwned(context.Background(), db, op, lease, func(ctx context.Context) error {
		timer := time.NewTimer(2 * lease)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		if started, _, err := TryStartWithToken(db, op.ID, lease); err != nil || started {
			return errors.New("slow provider lost ownership to recovery")
		}
		return MarkSucceededOwned(db, op.ID, token)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStaleFailureCannotHideLostLeaseWhenReplacementFailed(t *testing.T) {
	db := testDB(t)
	op, err := Enqueue(db, Input{OperationType: MultipartAbort, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Delete(&Operation{}, "id = ?", op.ID) })
	_, old, _ := TryStartWithToken(db, op.ID, time.Minute)
	if err := db.Model(&Operation{}).Where("id = ?", op.ID).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	_, replacement, _ := TryStartWithToken(db, op.ID, time.Minute)
	if err := MarkFailedOwned(db, op.ID, replacement, errors.New("replacement"), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := MarkFailedOwned(db, op.ID, old, errors.New("stale"), time.Second); err == nil {
		t.Fatal("stale failure reported success")
	}
	current, _ := Get(db, op.ID)
	if current.LastError == nil || *current.LastError != "replacement" {
		t.Fatalf("stale failure overwrote replacement: %+v", current)
	}
}

func TestCanceledWorkerDoesNotSpendDurableOperationBudget(t *testing.T) {
	db := testDB(t)
	op, err := Enqueue(db, Input{OperationType: MultipartAbort, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Delete(&Operation{}, "id = ?", op.ID) })
	for range MaxAttempts + 1 {
		started, token, err := TryStartWithToken(db, op.ID, time.Minute)
		if err != nil || !started {
			t.Fatal(started, err)
		}
		op.LeaseToken = &token
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := RecordError(ctx, db, op, context.Canceled); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	current, err := Get(db, op.ID)
	if err != nil || current.State != StatePending || current.Attempt != 0 {
		t.Fatalf("shutdown consumed budget: %+v %v", current, err)
	}
}
