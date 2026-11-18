package upload

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

// Wait for evidence from PG, rather than guessing lock order with a sleep.
func waitSessionWaiter(t *testing.T, db *gorm.DB, blocker int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := db.Raw(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked).Error; err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("request never waited on the session's PG row lock")
}

func lockSession(t *testing.T, f sessionHTTPFixture, id uuid.UUID) (*gorm.DB, *Service, int) {
	t.Helper()
	tx := f.db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	if err := tx.Exec("SELECT id FROM upload_sessions WHERE id=? FOR UPDATE", id).Error; err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
		t.Fatal(err)
	}
	service := NewRecoveryService(tx, f.service.objects, f.service.quota)
	return tx, service, pid
}

func TestSessionHTTPRowLockFencesTransitions(t *testing.T) {
	for _, name := range []string{"confirm_before_complete", "complete_before_replay", "abort_before_confirm", "confirm_before_abort", "abort_before_complete", "complete_before_abort", "expiry_before_confirm", "complete_before_expiry"} {
		t.Run(name, func(t *testing.T) {
			f := newSessionHTTPFixture(t)
			s := f.ready(t, f.create(t, "fence.bin", 128, uuid.NewString()))
			path := "/" + s.ID.String()
			if name == "complete_before_replay" || name == "abort_before_complete" || name == "complete_before_abort" || name == "complete_before_expiry" {
				requireHTTP(t, f.confirm(s.ID, partJSON(1, 128, "one")), http.StatusOK)
			}
			if name == "expiry_before_confirm" {
				if err := f.db.Model(&Session{}).Where("id=?", s.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
			}
			tx, winner, pid := lockSession(t, f, s.ID)
			result := make(chan *httptest.ResponseRecorder, 1)
			expiry := make(chan bool, 1)
			go func() {
				switch name {
				case "confirm_before_complete", "abort_before_complete":
					result <- f.request(http.MethodPost, path+"/complete", nil, "")
				case "confirm_before_abort", "complete_before_abort":
					result <- f.request(http.MethodDelete, path, nil, "")
				case "complete_before_expiry":
					changed, err := f.service.expireOne(context.Background(), s.ID)
					if err != nil {
						t.Error(err)
					}
					expiry <- changed
				default:
					result <- f.confirm(s.ID, partJSON(1, 128, "one"))
				}
			}()
			waitSessionWaiter(t, f.db, pid)
			var err error
			want, status := http.StatusConflict, StatusAborted
			switch name {
			case "confirm_before_complete", "confirm_before_abort":
				_, err = winner.ConfirmPartsContext(context.Background(), f.userID, s.ID, []ConfirmedPart{{PartNumber: 1, SizeBytes: 128, ETag: "one"}}, true)
				if name == "confirm_before_complete" {
					want, status = http.StatusAccepted, StatusCompleting
				} else {
					want = http.StatusNoContent
				}
			case "complete_before_replay", "complete_before_abort", "complete_before_expiry":
				_, err = winner.Complete(context.Background(), f.userID, s.ID)
				status = StatusCompleting
				if name == "complete_before_replay" {
					want = http.StatusOK
				}
				if name == "complete_before_expiry" && err == nil {
					err = tx.Model(&Session{}).Where("id=?", s.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error
				}
			case "expiry_before_confirm":
				_, err = winner.expireOne(context.Background(), s.ID)
				status = StatusExpired
			default:
				err = winner.Abort(context.Background(), f.userID, s.ID)
			}
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			if name == "complete_before_expiry" {
				select {
				case changed := <-expiry:
					if changed {
						t.Fatal("expiry interrupted accepted merge")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("expiry did not finish")
				}
			} else {
				select {
				case w := <-result:
					requireHTTP(t, w, want)
				case <-time.After(3 * time.Second):
					t.Fatal("blocked request did not finish")
				}
			}
			got := f.stored(t, s.ID)
			if got.Status != status {
				t.Fatalf("status=%s want %s", got.Status, status)
			}
			if (status == StatusExpired || name == "abort_before_confirm") && len(got.ConfirmedParts) != 0 {
				t.Fatal("late confirmation crossed terminal fence")
			}
			if status != StatusCompleting && f.occupied(t) != 0 {
				t.Fatal("terminal transition leaked reservation")
			}
			if status == StatusCompleting && f.operationCount(t, s.ID, storageops.MultipartComplete) != 1 {
				t.Fatal("merge intent count")
			}
		})
	}
}

func TestSessionCanceledConfirmationCannotWriteAfterLockRelease(t *testing.T) {
	f := newSessionHTTPFixture(t)
	s := f.ready(t, f.create(t, "timeout.bin", 128, uuid.NewString()))
	tx, _, pid := lockSession(t, f, s.ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.service.ConfirmPartsContext(ctx, f.userID, s.ID, []ConfirmedPart{{PartNumber: 1, SizeBytes: 128, ETag: "one"}}, true)
		done <- err
	}()
	waitSessionWaiter(t, f.db, pid)
	cancel()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("cancellation masked: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled query retained row waiter")
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if got := f.stored(t, s.ID); got.Status != StatusCreated || len(got.ConfirmedParts) != 0 {
		t.Fatal("canceled request committed later")
	}
	requireHTTP(t, f.confirm(s.ID, partJSON(1, 128, "one")), http.StatusOK)
}

func TestSessionBatchDatabaseFailureRollsBackAndCanRetry(t *testing.T) {
	f := newSessionHTTPFixture(t)
	s := f.ready(t, f.create(t, "rollback.bin", 20*1024*1024, uuid.NewString()))
	if err := f.db.Exec(`CREATE FUNCTION reject_second_part() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.part_number=2 THEN RAISE EXCEPTION 'injected second part failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_second_part BEFORE INSERT ON upload_parts FOR EACH ROW EXECUTE FUNCTION reject_second_part()`).Error; err != nil {
		t.Fatal(err)
	}
	parts := []map[string]any{partJSON(1, s.PartSize, "one"), partJSON(2, s.PartSize, "two")}
	for range 4 {
		requireHTTP(t, f.confirm(s.ID, parts...), http.StatusInternalServerError)
	}
	if got := f.stored(t, s.ID); len(got.ConfirmedParts) != 0 || got.Status != StatusCreated || f.occupied(t) != s.SizeBytes {
		t.Fatal("exhausted control requests partially committed or terminated recoverable session")
	}
	if err := f.db.Exec("DROP TRIGGER reject_second_part ON upload_parts").Error; err != nil {
		t.Fatal(err)
	}
	requireHTTP(t, f.confirm(s.ID, parts...), http.StatusOK)
}

func TestSessionExpiredAbortPreservesTerminalConclusion(t *testing.T) {
	f := newSessionHTTPFixture(t)
	s := f.ready(t, f.create(t, "expired.bin", 128, uuid.NewString()))
	if err := f.db.Model(&Session{}).Where("id=?", s.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.expireOne(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	before := f.stored(t, s.ID)
	for range 2 {
		requireHTTP(t, f.request(http.MethodDelete, "/"+s.ID.String(), nil, ""), http.StatusNoContent)
	}
	after := f.stored(t, s.ID)
	if after.Status != StatusExpired || after.Version != before.Version || f.occupied(t) != 0 {
		t.Fatalf("DELETE changed expired conclusion: before=%s/v%d after=%s/v%d", before.Status, before.Version, after.Status, after.Version)
	}
	for _, kind := range []string{storageops.MultipartAbort, "multipart.sweep", storageops.StagingDelete} {
		if f.operationCount(t, s.ID, kind) != 1 {
			t.Fatalf("expiry did not schedule %s exactly once", kind)
		}
	}
}

func TestSessionHTTPCompleteDatabaseFailureRemainsRetryable(t *testing.T) {
	f := newSessionHTTPFixture(t)
	s := f.ready(t, f.create(t, "complete-db-error.bin", 128, uuid.NewString()))
	requireHTTP(t, f.confirm(s.ID, partJSON(1, 128, "one")), http.StatusOK)
	if err := f.db.Exec(`CREATE FUNCTION reject_complete_transition() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='completing' THEN RAISE EXCEPTION 'injected complete update failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_complete_transition BEFORE UPDATE ON upload_sessions FOR EACH ROW EXECUTE FUNCTION reject_complete_transition()`).Error; err != nil {
		t.Fatal(err)
	}
	w := f.request(http.MethodPost, "/"+s.ID.String()+"/complete", nil, "")
	requireHTTP(t, w, http.StatusInternalServerError)
	if f.stored(t, s.ID).Status != StatusUploading || f.operationCount(t, s.ID, storageops.MultipartComplete) != 0 || f.occupied(t) != 128 {
		t.Fatal("failed complete left a state transition or merge intent")
	}
	if err := f.db.Exec("DROP TRIGGER reject_complete_transition ON upload_sessions").Error; err != nil {
		t.Fatal(err)
	}
	requireHTTP(t, f.request(http.MethodPost, "/"+s.ID.String()+"/complete", nil, ""), http.StatusAccepted)
}
