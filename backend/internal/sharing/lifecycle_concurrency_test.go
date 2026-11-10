package sharing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
)

func waitForRowLock(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := db.Raw(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity
            WHERE datname = current_database() AND wait_event_type = 'Lock'
            AND query ILIKE ? AND pid <> pg_backend_pid())`, "%"+table+"%").Scan(&waiting).Error
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("request never waited for the share lock")
}

func waitForDatabaseTime(t *testing.T, db *gorm.DB, until time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		now, err := databaseNow(db)
		if err != nil {
			t.Fatal(err)
		}
		if !now.Before(until) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("database clock did not reach deadline")
}

func TestShareRequestsRecheckExpiryAfterLockWait(t *testing.T) {
	for _, operation := range []string{"update", "revoke", "resolve", "access"} {
		t.Run(operation, func(t *testing.T) {
			f := seedShare(t, true)
			share, key, err := f.s.Create(f.owner, f.file.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, token, _, err := f.s.Resolve(key)
			if err != nil {
				t.Fatal(err)
			}
			now, err := databaseNow(f.db)
			if err != nil {
				t.Fatal(err)
			}
			expiry := now.Add(300 * time.Millisecond)
			tx := f.db.Begin()
			defer tx.Rollback()
			if err := tx.Model(&Share{}).Where("id = ?", share.ID).Update("expires_at", expiry).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "update":
					err = f.s.UpdateExpiry(ctx, f.owner, share.ID, 1, nil)
				case "revoke":
					err = f.s.RevokeWithContext(ctx, f.owner, share.ID)
				case "resolve":
					_, _, _, _, err = f.s.ResolveWithContext(ctx, key)
				case "access":
					_, _, err = f.s.AccessWithContext(ctx, token)
				}
				done <- err
			}()
			waitForRowLock(t, f.db, "shares")
			waitForDatabaseTime(t, f.db, expiry)
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			want := ErrNotFound
			if operation == "update" || operation == "revoke" {
				want = ErrInactive
			}
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("after lock wait: %v, want %v", err, want)
			}
			view, err := f.s.GetView(ctx, f.owner, share.ID)
			if err != nil || view.Version != 1 || view.RevokedAt != nil || view.Status != "expired" {
				t.Fatalf("expired record mutated: %+v %v", view, err)
			}
		})
	}
}

func TestShareNewDeadlineRecheckedAfterSourceLockWait(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			f := seedShare(t, true)
			share, _, err := f.s.Create(f.owner, f.file.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			tx := f.db.Begin()
			defer tx.Rollback()
			if err := tx.Exec("SELECT 1 FROM file_entries WHERE id = ? FOR UPDATE", f.file.ID).Error; err != nil {
				t.Fatal(err)
			}
			now, err := databaseNow(f.db)
			if err != nil {
				t.Fatal(err)
			}
			expiry := now.Add(300 * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if operation == "create" {
					_, _, err := f.s.CreateWithContext(ctx, f.owner, f.file.ID, &expiry)
					done <- err
				} else {
					done <- f.s.UpdateExpiry(ctx, f.owner, share.ID, 1, &expiry)
				}
			}()
			// Both operations must wait on the independently locked source row.
			waitForRowLock(t, f.db, "file_entries")
			waitForDatabaseTime(t, f.db, expiry)
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("accepted past deadline: %v", err)
			}
			view, err := f.s.GetView(ctx, f.owner, share.ID)
			if err != nil || view.Version != 1 || view.ExpiresAt != nil {
				t.Fatalf("failed edit changed share: %+v %v", view, err)
			}
		})
	}
}

func TestShareAccessWaitsForRevocationAndShortening(t *testing.T) {
	for _, operation := range []string{"revoke", "shorten"} {
		t.Run(operation, func(t *testing.T) {
			f := seedShare(t, true)
			share, key, err := f.s.Create(f.owner, f.file.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, token, _, err := f.s.Resolve(key)
			if err != nil {
				t.Fatal(err)
			}
			tx := f.db.Begin()
			defer tx.Rollback()
			field := "revoked_at"
			if operation == "shorten" {
				field = "expires_at"
			}
			if err := tx.Model(&Share{}).Where("id = ?", share.ID).Update(field, gorm.Expr("clock_timestamp() - interval '1 second'")).Error; err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() { _, _, err := f.s.AccessWithContext(ctx, token); done <- err }()
			waitForRowLock(t, f.db, "shares")
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrNotFound) {
				t.Fatalf("stale authorization: %v", err)
			}
		})
	}
}

func TestShareConcurrentEditAndRevokeCannotRestoreShare(t *testing.T) {
	f := seedShare(t, true)
	ctx := outbox.WithV2EventsEnabled(context.Background(), true)
	for i := 0; i < 12; i++ {
		share, key, err := f.s.Create(f.owner, f.file.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make(chan error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); <-start; results <- f.s.UpdateExpiry(ctx, f.owner, share.ID, 1, nil) }()
		go func() { defer wg.Done(); <-start; results <- f.s.RevokeWithContext(ctx, f.owner, share.ID) }()
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil && !errors.Is(err, ErrInactive) {
				t.Fatal(err)
			}
		}
		view, err := f.s.GetView(ctx, f.owner, share.ID)
		if err != nil || view.RevokedAt == nil {
			t.Fatalf("revocation lost: %+v %v", view, err)
		}
		if _, _, _, _, err := f.s.Resolve(key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("revoked share accessible: %v", err)
		}
	}
}

func TestShareLifecycleFilteredPagination(t *testing.T) {
	f := seedShare(t, false)
	ctx := context.Background()
	for _, status := range []string{"active", "expired", "revoked", "source_deleted"} {
		for i := 0; i < 13; i++ {
			share, _, err := f.s.Create(f.owner, f.file.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if status == "expired" {
				f.db.Model(&share).Update("expires_at", gorm.Expr("clock_timestamp() - interval '1 second'"))
			}
			if status == "source_deleted" {
				if err := f.db.Model(&share).Update("source_deleted_at", gorm.Expr("clock_timestamp()")).Error; err != nil {
					t.Fatal(err)
				}
			}
			if status == "revoked" {
				if err := f.s.Revoke(f.owner, share.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, status := range []string{"active", "expired", "revoked", "source_deleted"} {
		first, next, err := f.s.ListPage(ctx, f.owner, status, "", 10)
		if err != nil || len(first) != 10 || next == "" {
			t.Fatalf("first %s: %d %v", status, len(first), err)
		}
		second, last, err := f.s.ListPage(ctx, f.owner, status, next, 10)
		if err != nil || len(second) != 3 || last != "" {
			t.Fatalf("second %s: %d %v", status, len(second), err)
		}
		seen := map[uuid.UUID]bool{}
		for _, item := range append(first, second...) {
			if item.Status != status || seen[item.ID] {
				t.Fatalf("wrong state or duplicate: %+v", item)
			}
			seen[item.ID] = true
		}
		if _, _, err := f.s.ListPage(ctx, f.owner, "expired", next, 10); status != "expired" && !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("cross-filter cursor: %v", err)
		}
	}
	if _, _, err := f.s.ListPage(ctx, f.owner, "other", "", 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown filter: %v", err)
	}
}

func TestShareExactDeadlineIsInactive(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		expiry := now.Add(offset)
		err := validateShareAt(Share{ExpiresAt: &expiry}, now)
		if (err == nil) != (offset > 0) {
			t.Fatalf("offset=%v err=%v", offset, err)
		}
	}
}

func TestShareExpiryRecheckedAfterOwnerEpochLockWait(t *testing.T) {
	for _, operation := range []string{"update", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			f := seedShare(t, true)
			now, err := databaseNow(f.db)
			if err != nil {
				t.Fatal(err)
			}
			expiry := now.Add(400 * time.Millisecond)
			share, _, err := f.s.Create(f.owner, f.file.ID, &expiry)
			if err != nil {
				t.Fatal(err)
			}
			var before int64
			if err := f.db.Raw("SELECT acl_epoch FROM owner_content_epochs WHERE owner_id = ?", f.owner).Scan(&before).Error; err != nil {
				t.Fatal(err)
			}
			tx := f.db.Begin()
			defer tx.Rollback()
			if err := tx.Exec("SELECT 1 FROM owner_content_epochs WHERE owner_id = ? FOR UPDATE", f.owner).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if operation == "update" {
					done <- f.s.UpdateExpiry(ctx, f.owner, share.ID, 1, nil)
				} else {
					done <- f.s.RevokeWithContext(ctx, f.owner, share.ID)
				}
			}()
			waitForRowLock(t, f.db, "owner_content_epochs")
			waitForDatabaseTime(t, f.db, expiry)
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrInactive) {
				t.Fatalf("mutated after epoch wait: %v", err)
			}
			var after int64
			if err := f.db.Raw("SELECT acl_epoch FROM owner_content_epochs WHERE owner_id = ?", f.owner).Scan(&after).Error; err != nil {
				t.Fatal(err)
			}
			view, err := f.s.GetView(ctx, f.owner, share.ID)
			if err != nil || view.Version != 1 || view.RevokedAt != nil || after != before {
				t.Fatalf("failed mutation changed state: %+v epochs=%d/%d err=%v", view, before, after, err)
			}
		})
	}
}

func TestShareImportReauthorizesAfterIdempotencyLockWait(t *testing.T) {
	for _, operation := range []string{"token-expiry", "revocation"} {
		t.Run(operation, func(t *testing.T) {
			f := seedShare(t, true)
			share, key, err := f.s.Create(f.owner, f.file.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, token, _, err := f.s.Resolve(key)
			if err != nil {
				t.Fatal(err)
			}
			now, err := databaseNow(f.db)
			if err != nil {
				t.Fatal(err)
			}
			tokenExpiry := time.Unix(now.Unix()+2, 0)
			if operation == "token-expiry" {
				token, err = f.s.signAccess(share.ID, tokenExpiry)
				if err != nil {
					t.Fatal(err)
				}
			}
			root, err := f.s.drive.Root(f.owner)
			if err != nil {
				t.Fatal(err)
			}
			idempotency := uuid.NewString()
			tx := f.db.Begin()
			defer tx.Rollback()
			lockKey := "share:import:" + f.owner.String() + ":" + share.ID.String() + ":" + idempotency
			if err := dblock.Transaction(tx, lockKey); err != nil {
				t.Fatal(err)
			}
			f.s.quota = failedQuota{errors.New("unauthorized request reached quota update")}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := f.s.Import(ctx, f.owner, share.ID, root.ID, idempotency, token); done <- err }()
			waitForRowLock(t, f.db, "pg_advisory")
			if operation == "token-expiry" {
				waitForDatabaseTime(t, f.db, tokenExpiry)
			} else if err := f.s.Revoke(f.owner, share.ID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrNotFound) {
				t.Fatalf("import authorized stale state: %v", err)
			}
			var count int64
			if err := f.db.Model(&Import{}).Where("share_id = ?", share.ID).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("unauthorized import persisted: count=%d err=%v", count, err)
			}
		})
	}
}
