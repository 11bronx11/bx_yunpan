package storageops

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func testDB(t *testing.T) *gorm.DB {
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

func TestEnqueueIsIdempotentAndRollbackSafe(t *testing.T) {
	db := testDB(t)
	key := "storageops-test:" + uuid.NewString()
	err := db.Transaction(func(tx *gorm.DB) error {
		first, err := Enqueue(tx, Input{OperationType: StagingDelete, IdempotencyKey: key, SourceBucket: "test", SourceKey: "staging/test"})
		if err != nil {
			return err
		}
		second, err := Enqueue(tx, Input{OperationType: StagingDelete, IdempotencyKey: key, SourceBucket: "changed", SourceKey: "changed"})
		if err != nil {
			return err
		}
		if first.ID != second.ID || second.SourceBucket != "test" {
			return errors.New("idempotency key created or replaced an operation")
		}
		return errors.New("rollback operation")
	})
	if err == nil || err.Error() != "rollback operation" {
		t.Fatalf("transaction result = %v", err)
	}
	var count int64
	if err := db.Model(&Operation{}).Where("idempotency_key = ?", key).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled back operations = %d", count)
	}
}

func TestClaimUsesLeaseAndMarksCompletion(t *testing.T) {
	db := testDB(t)
	key := "storageops-claim:" + uuid.NewString()
	op, err := Enqueue(db, Input{OperationType: StagingDelete, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&Operation{}, "id = ?", op.ID).Error })
	claimed, err := ClaimTypes(context.Background(), db, 1, time.Minute, StagingDelete)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != op.ID || claimed[0].State != StateRunning || claimed[0].Attempt != 1 {
		t.Fatalf("claimed operations = %#v", claimed)
	}
	if err := MarkSucceeded(db, op.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = ClaimTypes(context.Background(), db, 1, time.Minute, StagingDelete)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("succeeded operation was claimed again: %#v", claimed)
	}
}

func TestExpiredLeaseCannotOverwriteReplacementWorker(t *testing.T) {
	db := testDB(t)
	key := "storageops-fence:" + uuid.NewString()
	op, err := Enqueue(db, Input{OperationType: StagingDelete, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&Operation{}, "id = ?", op.ID).Error })
	if err := db.Model(&Operation{}).Where("id = ?", op.ID).Updates(map[string]any{
		"state": StateRunning, "lease_until": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	first, token1, err := TryStartWithToken(db, op.ID, time.Minute)
	if err != nil || !first {
		t.Fatalf("first claim = %t, err = %v", first, err)
	}
	if err := db.Model(&Operation{}).Where("id = ?", op.ID).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	second, token2, err := TryStartWithToken(db, op.ID, time.Minute)
	if err != nil || !second || token1 == token2 {
		t.Fatalf("replacement claim = %t, token1=%s token2=%s err=%v", second, token1, token2, err)
	}
	if err := MarkSucceededOwned(db, op.ID, token1); err == nil {
		t.Fatal("expired lease owner completed operation")
	}
	if err := MarkSucceededOwned(db, op.ID, token2); err != nil {
		t.Fatal(err)
	}
	current, err := Get(db, op.ID)
	if err != nil || current.State != StateSucceeded {
		t.Fatalf("operation = %#v, err = %v", current, err)
	}
}
