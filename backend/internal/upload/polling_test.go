package upload

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestGetContextPreservesCancellationAndDatabaseErrors(t *testing.T) {
	for _, cause := range []error{gorm.ErrRecordNotFound, context.Canceled, errors.New("database unavailable")} {
		t.Run(cause.Error(), func(t *testing.T) {
			db, err := gorm.Open(postgres.Open("host=127.0.0.1 port=1 user=test dbname=test sslmode=disable"), &gorm.Config{
				DisableAutomaticPing: true, DryRun: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			pool, _ := db.DB()
			t.Cleanup(func() { _ = pool.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if errors.Is(cause, context.Canceled) {
				cancel()
			}
			if err := db.Callback().Query().Before("gorm:query").Register("test:query_error", func(tx *gorm.DB) {
				if tx.Statement.Context != ctx {
					t.Error("query did not receive the request context")
				}
				tx.AddError(cause)
			}); err != nil {
				t.Fatal(err)
			}
			_, err = (&Service{db: db}).GetContext(ctx, uuid.New(), uuid.New())
			want := cause
			if errors.Is(cause, gorm.ErrRecordNotFound) {
				want = ErrNotFound
			}
			if !errors.Is(err, want) {
				t.Fatalf("query error = %v, want %v", err, want)
			}
		})
	}
}
