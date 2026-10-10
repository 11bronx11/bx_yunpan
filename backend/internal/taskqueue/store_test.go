package taskqueue

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"os"
	"testing"
)

func TestStoreSuppressesSuccessAndTerminalRedelivery(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	t.Cleanup(func() { _ = sql.Close() })
	s := NewStore(db)
	ctx := context.Background()
	success := NewTask("kind", []byte(`{}`))
	success.ID = uuid.NewString()
	dead := NewTask("poison", []byte("not-json"))
	dead.ID = uuid.NewString()
	t.Cleanup(func() {
		db.Delete(&Execution{}, "task_id IN ?", []string{success.ID, dead.ID})
		db.Delete(&Result{}, "task_id IN ?", []string{success.ID, dead.ID})
		db.Delete(&Failure{}, "task_id = ?", dead.ID)
	})
	calls := 0
	h := s.Middleware()(HandlerFunc(func(context.Context, *Task) error { calls++; return nil }))
	for i := 0; i < 2; i++ {
		if err := h.ProcessTask(ctx, success); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordFailure(ctx, *dead, errors.New("poison")); err != nil {
		t.Fatal(err)
	}
	if err := h.ProcessTask(ctx, dead); !errors.Is(err, SkipRetry) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("repeated side effect: %d", calls)
	}
	var failure Failure
	if err := db.First(&failure, "task_id = ?", dead.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(failure.Payload) != "not-json" {
		t.Fatal("poison payload lost")
	}
}
