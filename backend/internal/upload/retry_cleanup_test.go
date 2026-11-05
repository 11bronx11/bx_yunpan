package upload

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
)

func TestStoppedUploadSessionReleasesQuotaAndSchedulesCleanupOnce(t *testing.T) {
	for _, status := range []string{StatusInitializing, StatusCompleting} {
		t.Run(status, func(t *testing.T) {
			f := newConcurrencyFixture(t)
			maintenance := NewRecoveryService(f.db, f.service.objects, f.service.quota)
			session := testActiveSession(f, time.Now().Add(time.Hour))
			session.Status = status
			if err := f.db.Create(&session).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Model(&identity.User{}).Where("id = ?", f.userID).Update("reserved_bytes", session.ReservedBytes).Error; err != nil {
				t.Fatal(err)
			}
			kind := storageops.MultipartCreate
			if status == StatusCompleting {
				kind = storageops.MultipartComplete
			}
			op, err := storageops.Enqueue(f.db, storageops.Input{OperationType: kind, IdempotencyKey: uuid.NewString(), UploadSessionID: &session.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.Model(&op).Updates(map[string]any{"state": storageops.StateDead, "attempt": storageops.MaxAttempts}).Error; err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := maintenance.RetireStoppedSessions(context.Background(), 100); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			var stored Session
			if err := f.db.First(&stored, "id = ?", session.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.Status != StatusFailed || stored.ReservedBytes != 0 || stored.ErrorCode == nil || *stored.ErrorCode != "upload.storage_operation_exhausted" {
				t.Fatalf("session=%+v", stored)
			}
			var user identity.User
			if err := f.db.First(&user, "id = ?", f.userID).Error; err != nil {
				t.Fatal(err)
			}
			if user.ReservedBytes != 0 {
				t.Fatal(user.ReservedBytes)
			}
			for _, kind := range []string{storageops.MultipartAbort, storageops.StagingDelete} {
				var n int64
				if err := f.db.Model(&storageops.Operation{}).Where("upload_session_id = ? AND operation_type = ?", session.ID, kind).Count(&n).Error; err != nil || n != 1 {
					t.Fatalf("cleanup %s=%d err=%v", kind, n, err)
				}
			}
			if f.storage.abortCount() != 0 {
				t.Fatal("maintenance executed provider cleanup")
			}
		})
	}
}

func TestMaintenanceFinishesVerificationWhoseTerminalTaskLostDomainUpdate(t *testing.T) {
	f := newConcurrencyFixture(t)
	maintenance := NewRecoveryService(f.db, f.service.objects, f.service.quota)
	session := newVerifyingSession(f.userID, f.rootID, "terminal-verify.txt", []byte(uuid.NewString()))
	if err := f.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&identity.User{}).Where("id = ?", f.userID).Update("reserved_bytes", session.ReservedBytes).Error; err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		return outbox.AddTyped(context.Background(), tx, eventcontract.EnvelopeInput{EventID: eventID, EventType: outbox.EventObjectVerifyRequested, AggregateID: session.ID, AggregateVersion: session.Version, Producer: "test", Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: session.ID}})
	}); err != nil {
		t.Fatal(err)
	}
	taskID := eventID.String() + ":" + outbox.TaskObjectVerifyV1 + "@object"
	if err := f.db.Create(&taskqueue.Result{TaskID: taskID, Status: "dead", FinishedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Delete(&taskqueue.Result{}, "task_id = ?", taskID) })
	if n, err := maintenance.RetireStoppedSessions(context.Background(), 100); err != nil || n != 1 {
		t.Fatalf("retired=%d err=%v", n, err)
	}
	var stored Session
	if err := f.db.First(&stored, "id = ?", session.ID).Error; err != nil || stored.Status != StatusFailed || stored.ReservedBytes != 0 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	var operation storageops.Operation
	if err := f.db.Where("upload_session_id = ? AND operation_type = ?", session.ID, storageops.StagingDelete).First(&operation).Error; err != nil || operation.State != storageops.StatePending {
		t.Fatalf("cleanup=%+v err=%v", operation, err)
	}
}

type selectiveReleaseFailure struct{ Quota }

func (q selectiveReleaseFailure) ReleaseQuota(tx *gorm.DB, userID uuid.UUID, size int64) error {
	if size == 128 {
		return errors.New("injected invalid account reservation")
	}
	return q.Quota.ReleaseQuota(tx, userID, size)
}

func TestExpiryFailureDoesNotStarveOtherSessions(t *testing.T) {
	for _, batch := range []int{1, 10} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			f := newConcurrencyFixture(t)
			maintenance := NewRecoveryService(f.db, f.service.objects, selectiveReleaseFailure{f.service.quota})
			broken := testActiveSession(f, time.Now().Add(-time.Hour))
			broken.ID = uuid.Must(uuid.NewV7())
			healthy := testActiveSession(f, time.Now().Add(-time.Hour))
			healthy.ID = uuid.Must(uuid.NewV7())
			healthy.Filename = "healthy.txt"
			healthy.FilenameNormalized = healthy.Filename
			healthy.SizeBytes = 256
			healthy.ReservedBytes = 256
			for _, session := range []Session{broken, healthy} {
				if err := f.db.Create(&session).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := f.db.Model(&identity.User{}).Where("id = ?", f.userID).Update("reserved_bytes", 384).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := maintenance.Expire(context.Background(), batch); err == nil {
				t.Fatal("broken reservation was hidden")
			}
			if batch == 1 {
				if n, err := maintenance.Expire(context.Background(), 1); err != nil || n != 1 {
					t.Fatalf("next page n=%d err=%v", n, err)
				}
			}
			var stored Session
			if err := f.db.First(&stored, "id = ?", healthy.ID).Error; err != nil || stored.Status != StatusExpired {
				t.Fatalf("healthy session starved: %+v %v", stored, err)
			}
		})
	}
}

type delayedCopyStorage struct {
	*fakeUploadStorage
	content []byte
	entered chan struct{}
	release chan struct{}
}

func (s *delayedCopyStorage) CopyObject(ctx context.Context, dst minio.CopyDestOptions, src minio.CopySrcOptions) (minio.UploadInfo, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return minio.UploadInfo{}, ctx.Err()
	}
	s.put(dst.Bucket, dst.Object, s.content)
	return minio.UploadInfo{}, nil
}

func TestGCWaitsForCanonicalCopyReturningAfterVerificationFailure(t *testing.T) {
	f := newConcurrencyFixture(t)
	content := []byte("late canonical copy " + uuid.NewString())
	store := &delayedCopyStorage{fakeUploadStorage: newFakeUploadStorage(), content: content, entered: make(chan struct{}), release: make(chan struct{})}
	f.service.storage = store
	session := newVerifyingSession(f.userID, f.rootID, "late-copy.txt", content)
	if err := f.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&identity.User{}).Where("id = ?", f.userID).Update("reserved_bytes", session.ReservedBytes).Error; err != nil {
		t.Fatal(err)
	}
	store.put(session.Bucket, session.ObjectKey, content)
	done := make(chan error, 1)
	go func() {
		_, err := f.service.ensureCanonical(context.Background(), session, session.DeclaredSHA256, session.SizeBytes)
		done <- err
	}()
	<-store.entered
	var release sync.Once
	unblock := func() { release.Do(func() { close(store.release) }) }
	t.Cleanup(unblock)
	if err := f.service.FailVerification(context.Background(), session.ID, "injected.failure"); err != nil {
		t.Fatal(err)
	}
	var object objectstore.Object
	if err := f.db.Where("sha256 = ?", session.DeclaredSHA256).First(&object).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.objects.GarbageCollect(context.Background(), store, object.ID); err == nil {
		t.Fatal("GC ran before canonical copy finished")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !store.exists(object.Bucket, object.ObjectKey) {
		t.Fatal("copy did not create expected late bytes")
	}
	if err := f.db.Model(&storageops.Operation{}).Where("object_id = ? AND operation_type = ?", object.ID, storageops.ObjectDelete).Update("available_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.objects.GarbageCollect(context.Background(), store, object.ID); err != nil {
		t.Fatal(err)
	}
	if store.exists(object.Bucket, object.ObjectKey) {
		t.Fatal("late copy escaped GC")
	}
	if err := f.db.First(&object, "id = ?", object.ID).Error; err != nil || object.Status != "deleted" {
		t.Fatalf("object=%+v err=%v", object, err)
	}
}
