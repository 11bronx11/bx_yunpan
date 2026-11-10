package sharing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
)

func fileManager(db *gorm.DB, quota drive.LogicalUsage) *drive.FileManager {
	d := drive.NewService(db)
	return drive.NewFileManager(d, objectstore.NewService(db), quota, nil, nil, 0)
}
func deleteFixture(t *testing.T) (shareFixture, *drive.FileManager) {
	t.Helper()
	f := seedShare(t, true)
	quota := identity.NewService(f.db, nil, f.s.drive, 10*1024*1024*1024)
	f.s.quota = quota
	if err := f.db.Model(&identity.User{}).Where("id = ?", f.owner).Update("used_logical_bytes", 128).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.db.Exec("DELETE FROM outbox_events WHERE aggregate_id IN (?, ?)", f.file.ID, f.file.ObjectID)
		f.db.Exec("DELETE FROM share_imports WHERE share_id IN (SELECT id FROM shares WHERE owner_id = ?)", f.owner)
	})
	return f, fileManager(f.db, quota)
}
func recipient(t *testing.T, f shareFixture) (uuid.UUID, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	if err := f.db.Exec("INSERT INTO users(id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'test')", id, id.String(), id.String()).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.drive.ProvisionUser(f.db, id); err != nil {
		t.Fatal(err)
	}
	root, err := f.s.drive.Root(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.db.Exec("DELETE FROM outbox_events WHERE aggregate_id IN (SELECT id FROM file_entries WHERE owner_id = ?)", id)
		f.db.Exec("DELETE FROM share_imports WHERE user_id = ?", id)
		f.db.Exec("DELETE FROM users WHERE id = ?", id)
	})
	return id, root.ID
}
func TestSourceDeletionPreservesIndependentShareHistory(t *testing.T) {
	f, manager := deleteFixture(t)
	ctx := context.Background()
	var shares []Share
	var keys, tokens []string
	for _, status := range []string{"active", "expired", "revoked"} {
		share, key, err := f.s.Create(f.owner, f.file.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, token, _, err := f.s.Resolve(key)
		if err != nil {
			t.Fatal(err)
		}
		if status == "expired" {
			if err := f.db.Model(&share).Update("expires_at", gorm.Expr("clock_timestamp() - interval '1 second'")).Error; err != nil {
				t.Fatal(err)
			}
		}
		if status == "revoked" {
			if err := f.s.Revoke(f.owner, share.ID); err != nil {
				t.Fatal(err)
			}
		}
		shares = append(shares, share)
		keys = append(keys, key)
		tokens = append(tokens, token)
	}
	renamed, err := f.s.drive.RenameFile(f.owner, f.file.ID, f.file.Version, "renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete(ctx, f.owner, f.file.ID, renamed.Version); err != nil {
		t.Fatal(err)
	}
	// History must still work after even the physical source row disappears.
	if err := f.db.Exec("DELETE FROM file_entries WHERE id = ?", f.file.ID).Error; err != nil {
		t.Fatal(err)
	}
	for i, share := range shares {
		view, err := f.s.GetView(ctx, f.owner, share.ID)
		if err != nil || view.FileName != f.file.Name || view.MimeType != "text/plain" || view.SourceDeletedAt == nil || view.Status != "source_deleted" || view.FileAvailable {
			t.Fatalf("history depends on source: %+v %v", view, err)
		}
		if _, exists := shareViewResponse(view)["size_bytes"]; exists {
			t.Fatal("history leaked physical object metadata")
		}
		if _, _, _, _, err := f.s.Resolve(keys[i]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted source resolved: %v", err)
		}
		if _, _, err := f.s.Access(tokens[i]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old token accessed deleted source: %v", err)
		}
		if err := f.s.UpdateExpiry(ctx, f.owner, share.ID, view.Version, nil); !errors.Is(err, ErrInactive) {
			t.Fatalf("deleted source renewed: %v", err)
		}
		if err := f.s.Revoke(f.owner, share.ID); !errors.Is(err, ErrInactive) {
			t.Fatalf("deleted history mutated: %v", err)
		}
	}
	for _, status := range []string{"active", "expired", "revoked", "source_deleted"} {
		rows, _, err := f.s.ListPage(ctx, f.owner, status, "", 10)
		want := 0
		if status == "source_deleted" {
			want = 3
		}
		if err != nil || len(rows) != want {
			t.Fatalf("%s: count=%d err=%v", status, len(rows), err)
		}
	}
}

type failingDeleteQuota struct{ err error }

func (q failingDeleteQuota) RemoveLogicalUsage(*gorm.DB, uuid.UUID, int64) error { return q.err }

func TestSourceDeletionRollsBackShareMarksAndReferences(t *testing.T) {
	f, _ := deleteFixture(t)
	share, _, err := f.s.Create(f.owner, f.file.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected quota failure")
	manager := fileManager(f.db, failingDeleteQuota{failure})
	if err := manager.Delete(context.Background(), f.owner, f.file.ID, f.file.Version); !errors.Is(err, failure) {
		t.Fatalf("delete error=%v", err)
	}
	view, err := f.s.GetView(context.Background(), f.owner, share.ID)
	if err != nil || view.SourceDeletedAt != nil || view.Version != 1 || view.Status != "active" {
		t.Fatalf("partial share commit: %+v %v", view, err)
	}
	var source drive.FileEntry
	if err := f.db.First(&source, "id = ?", f.file.ID).Error; err != nil || source.DeletedAt != nil || source.Version != f.file.Version {
		t.Fatalf("partial file commit: %+v %v", source, err)
	}
	var object objectstore.Object
	if err := f.db.First(&object, "id = ?", f.file.ObjectID).Error; err != nil || object.ReferenceCount != 1 {
		t.Fatalf("partial reference decrement: %+v %v", object, err)
	}
}

func TestConcurrentRepeatedSourceDeletionIsIdempotent(t *testing.T) {
	f, manager := deleteFixture(t)
	ctx := outbox.WithV2EventsEnabled(context.Background(), true)
	share, _, err := f.s.Create(f.owner, f.file.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- manager.Delete(ctx, f.owner, f.file.ID, f.file.Version) }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("repeat deletion: %v", err)
		}
	}
	view, err := f.s.GetView(ctx, f.owner, share.ID)
	if err != nil || view.Version != 2 || view.SourceDeletedAt == nil {
		t.Fatalf("share marked more than once: %+v %v", view, err)
	}
	var object objectstore.Object
	if err := f.db.First(&object, "id = ?", f.file.ObjectID).Error; err != nil || object.ReferenceCount != 0 || object.Version != 2 {
		t.Fatalf("duplicate reference decrement: %+v %v", object, err)
	}
	for _, event := range []struct {
		typ string
		id  uuid.UUID
	}{{eventcontract.EventFileDeletedV1, f.file.ID}, {outbox.EventObjectGCRequested, f.file.ObjectID}} {
		var count int64
		if err := f.db.Model(&outbox.Event{}).Where("aggregate_id = ? AND event_type = ?", event.id, event.typ).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("duplicate %s: count=%d err=%v", event.typ, count, err)
		}
	}
	var used int64
	if err := f.db.Raw("SELECT used_logical_bytes FROM users WHERE id = ?", f.owner).Scan(&used).Error; err != nil || used != 0 {
		t.Fatalf("quota after delete=%d err=%v", used, err)
	}
	if err := manager.Delete(ctx, uuid.New(), f.file.ID, f.file.Version); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("foreign delete=%v", err)
	}
}

func TestSourceDeletionLockOrderAgainstShareOperations(t *testing.T) {
	for _, operation := range []string{"create", "update", "revoke", "import"} {
		t.Run(operation, func(t *testing.T) {
			f, _ := deleteFixture(t)
			share, key, err := f.s.Create(f.owner, f.file.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, token, _, err := f.s.Resolve(key)
			if err != nil {
				t.Fatal(err)
			}
			user, folder := recipient(t, f)
			tx := f.db.Begin()
			defer tx.Rollback()
			if err := tx.Exec("SELECT 1 FROM file_entries WHERE id = ? FOR UPDATE", f.file.ID).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "create":
					_, _, err = f.s.CreateWithContext(ctx, f.owner, f.file.ID, nil)
				case "update":
					err = f.s.UpdateExpiry(ctx, f.owner, share.ID, 1, nil)
				case "revoke":
					err = f.s.RevokeWithContext(ctx, f.owner, share.ID)
				case "import":
					_, err = f.s.Import(ctx, user, share.ID, folder, uuid.NewString(), token)
				}
				done <- err
			}()
			waitForRowLock(t, f.db, "file_entries")
			// A waiting mutation must not hold a share lock needed by deletion.
			manager := fileManager(tx, identity.NewService(tx, nil, drive.NewService(tx), 10*1024*1024*1024))
			if err := manager.Delete(ctx, f.owner, f.file.ID, f.file.Version); err != nil {
				t.Fatalf("delete/share lock inversion: %v", err)
			}
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			want := ErrNotFound
			if operation == "update" || operation == "revoke" {
				want = ErrInactive
			}
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("request after deletion: %v want %v", err, want)
			}
		})
	}
}

func TestSourceDeletionWaitsForImportAndPreservesRecipient(t *testing.T) {
	f, manager := deleteFixture(t)
	share, key, err := f.s.Create(f.owner, f.file.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, token, _, err := f.s.Resolve(key)
	if err != nil {
		t.Fatal(err)
	}
	user, folder := recipient(t, f)
	tx := f.db.Begin()
	defer tx.Rollback()
	if err := tx.Exec("SELECT 1 FROM users WHERE id = ? FOR UPDATE", user).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	imported := make(chan drive.FileView, 1)
	imports := make(chan error, 1)
	go func() {
		file, err := f.s.Import(ctx, user, share.ID, folder, uuid.NewString(), token)
		imported <- file
		imports <- err
	}()
	waitForRowLock(t, f.db, "users")
	deleted := make(chan error, 1)
	go func() { deleted <- manager.Delete(ctx, f.owner, f.file.ID, f.file.Version) }()
	waitForRowLock(t, f.db, "file_entries")
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-imports; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	file := <-imported
	if _, err := f.s.drive.File(user, file.ID); err != nil {
		t.Fatalf("recipient lost imported file: %v", err)
	}
	var object objectstore.Object
	if err := f.db.First(&object, "id = ?", file.ObjectID).Error; err != nil || object.ReferenceCount != 1 {
		t.Fatalf("recipient object lost: %+v %v", object, err)
	}
	if _, _, err := f.s.Access(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source token still valid: %v", err)
	}
}

type gatedImportQuota struct {
	base     Quota
	acquired chan struct{}
	proceed  <-chan struct{}
}

func (q gatedImportQuota) AddLogicalUsage(tx *gorm.DB, user uuid.UUID, size int64) error {
	if err := q.base.AddLogicalUsage(tx, user, size); err != nil {
		return err
	}
	close(q.acquired)
	select {
	case <-q.proceed:
		return nil
	case <-tx.Statement.Context.Done():
		return tx.Statement.Context.Err()
	}
}

func TestDeletingAnotherFileCannotDeadlockRecipientImport(t *testing.T) {
	// Register the source first so the recipient's entries are cleaned first.
	source := seedShare(t, true)
	f, manager := deleteFixture(t)
	t.Cleanup(func() { f.db.Exec("DELETE FROM share_imports WHERE user_id = ?", f.owner) })
	root, err := f.s.drive.Root(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.s.drive.Create(f.owner, root.ID, "import-destination")
	if err != nil {
		t.Fatal(err)
	}
	share, key, err := source.s.Create(source.owner, source.file.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, token, _, err := source.s.Resolve(key)
	if err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(proceed) }) }
	defer release()
	source.s.quota = gatedImportQuota{identity.NewService(source.db, nil, source.s.drive, 10*1024*1024*1024), entered, proceed}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	imported := make(chan error, 1)
	go func() {
		_, err := source.s.Import(ctx, f.owner, share.ID, target.ID, uuid.NewString(), token)
		imported <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("import did not acquire recipient quota")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- manager.Delete(ctx, f.owner, f.file.ID, f.file.Version) }()
	// Delete now waits for the recipient quota without retaining an epoch lock.
	waitForRowLock(t, f.db, "users")
	release()
	if err := <-imported; err != nil {
		t.Fatalf("import/delete quota inversion: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete/import quota inversion: %v", err)
	}
	var used int64
	if err := f.db.Raw("SELECT used_logical_bytes FROM users WHERE id = ?", f.owner).Scan(&used).Error; err != nil || used != 128 {
		t.Fatalf("quota must retain only imported file: %d %v", used, err)
	}
}
