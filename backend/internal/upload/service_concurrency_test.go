package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

type fakeUploadStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	removed []string
}

func newFakeUploadStorage() *fakeUploadStorage {
	return &fakeUploadStorage{objects: make(map[string][]byte)}
}

func storageKey(bucket, object string) string { return bucket + "/" + object }

func (s *fakeUploadStorage) put(bucket, object string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[storageKey(bucket, object)] = append([]byte(nil), content...)
}

func (s *fakeUploadStorage) exists(bucket, object string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[storageKey(bucket, object)]
	return ok
}

func (s *fakeUploadStorage) GetObject(_ context.Context, bucket, object string, _ minio.GetObjectOptions) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.objects[storageKey(bucket, object)]
	if !ok {
		return nil, errors.New("object not found")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), content...))), nil
}

func (s *fakeUploadStorage) StatObject(_ context.Context, bucket, object string, _ minio.StatObjectOptions) (minio.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.objects[storageKey(bucket, object)]
	if !ok {
		return minio.ObjectInfo{}, errors.New("object not found")
	}
	return minio.ObjectInfo{Size: int64(len(content))}, nil
}

func (s *fakeUploadStorage) CopyObject(_ context.Context, destination minio.CopyDestOptions, source minio.CopySrcOptions) (minio.UploadInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.objects[storageKey(source.Bucket, source.Object)]
	if !ok {
		return minio.UploadInfo{}, errors.New("source object not found")
	}
	s.objects[storageKey(destination.Bucket, destination.Object)] = append([]byte(nil), content...)
	return minio.UploadInfo{}, nil
}

func (s *fakeUploadStorage) RemoveObject(_ context.Context, bucket, object string, _ minio.RemoveObjectOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storageKey(bucket, object)
	delete(s.objects, key)
	s.removed = append(s.removed, key)
	return nil
}

type commitFailQuota struct {
	Quota
	err error
}

func (q commitFailQuota) CommitQuota(*gorm.DB, uuid.UUID, int64, int64) error { return q.err }

type fakeMultipartStorage struct {
	mu            sync.Mutex
	newCalls      int
	completeCalls int
	abortCalls    int
}

func (s *fakeMultipartStorage) NewMultipartUpload(context.Context, string, string, minio.PutObjectOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newCalls++
	return "fake-upload-id", nil
}

func (s *fakeMultipartStorage) CompleteMultipartUpload(context.Context, string, string, string, []minio.CompletePart, minio.PutObjectOptions) (minio.UploadInfo, error) {
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	return minio.UploadInfo{}, nil
}

func (s *fakeMultipartStorage) AbortMultipartUpload(context.Context, string, string, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abortCalls++
	return nil
}

func (s *fakeMultipartStorage) calls() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newCalls, s.completeCalls
}

func (s *fakeMultipartStorage) abortCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abortCalls
}

type concurrencyFixture struct {
	db      *gorm.DB
	service *Service
	storage *fakeMultipartStorage
	userID  uuid.UUID
	rootID  uuid.UUID
}

func newConcurrencyFixture(t *testing.T) concurrencyFixture {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("load postgres pool: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	userID := uuid.New()
	username := "upload-concurrency-" + userID.String()
	if err := db.Exec(`
		INSERT INTO users (id, username, username_normalized, password_hash)
		VALUES (?, ?, ?, ?)`, userID, username, username, "test").Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	driveService := drive.NewService(db)
	if err := driveService.ProvisionUser(db, userID); err != nil {
		t.Fatalf("provision root: %v", err)
	}
	root, err := driveService.Root(userID)
	if err != nil {
		t.Fatalf("load root: %v", err)
	}
	storage := &fakeMultipartStorage{}
	service := NewService(db, nil, nil, "test", driveService, objectstore.NewService(db), identity.NewService(db, nil, driveService, 10*1024*1024*1024), config.Upload{
		SessionTTL: 24 * time.Hour, PartURLTTL: 15 * time.Minute, CleanupInterval: time.Minute, CleanupBatch: 100,
	})
	service.core = storage
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM outbox_events WHERE aggregate_type = 'upload_session' AND aggregate_id IN (SELECT id FROM upload_sessions WHERE user_id = ?)", userID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", userID).Error
	})
	return concurrencyFixture{db: db, service: service, storage: storage, userID: userID, rootID: root.ID}
}

func TestCreateSerializesConcurrentIdempotentRequests(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	digest := sha256.Sum256([]byte(uuid.NewString()))
	input := CreateInput{
		FolderID: fixture.rootID, Filename: "concurrent.txt", SHA256: hex.EncodeToString(digest[:]),
		SizeBytes: 128, MimeType: "text/plain", IdempotencyKey: uuid.NewString(),
	}

	const requests = 12
	results := make(chan CreateResult, requests)
	errs := make(chan error, requests)
	var wait sync.WaitGroup
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := fixture.service.Create(context.Background(), fixture.userID, input)
			results <- result
			errs <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("create upload: %v", err)
		}
	}
	var sessionID uuid.UUID
	for result := range results {
		if result.Mode != "multipart" || result.Upload == nil {
			t.Fatalf("unexpected result: %#v", result)
		}
		if sessionID == uuid.Nil {
			sessionID = result.Upload.ID
		} else if result.Upload.ID != sessionID {
			t.Fatalf("session ID = %s, want %s", result.Upload.ID, sessionID)
		}
	}
	newCalls, _ := fixture.storage.calls()
	if newCalls != 1 {
		t.Fatalf("multipart creations = %d, want 1", newCalls)
	}
	var count int64
	if err := fixture.db.Model(&Session{}).Where("user_id = ?", fixture.userID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("upload sessions = %d, err = %v", count, err)
	}
}

func TestCreateDistinguishesSameFolderNameConflict(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	firstDigest := sha256.Sum256([]byte("first-content"))
	secondDigest := sha256.Sum256([]byte("different-content"))
	first := CreateInput{
		FolderID: fixture.rootID, Filename: "main.cpp", SHA256: hex.EncodeToString(firstDigest[:]),
		SizeBytes: 128, MimeType: "application/octet-stream", IdempotencyKey: uuid.NewString(),
	}
	if _, err := fixture.service.Create(context.Background(), fixture.userID, first); err != nil {
		t.Fatalf("create first upload: %v", err)
	}
	second := first
	second.SHA256 = hex.EncodeToString(secondDigest[:])
	second.SizeBytes = 718
	second.IdempotencyKey = uuid.NewString()

	_, err := fixture.service.Create(context.Background(), fixture.userID, second)
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("create same-name upload error = %v, want ErrNameConflict", err)
	}
}

func TestCompleteSerializesConcurrentRetries(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	digest := sha256.Sum256([]byte(uuid.NewString()))
	session := Session{
		ID: uuid.New(), UserID: fixture.userID, FolderID: fixture.rootID, Filename: "complete.txt", FilenameNormalized: "complete.txt",
		DeclaredSHA256: hex.EncodeToString(digest[:]), MimeType: "text/plain", SizeBytes: 128, ReservedBytes: 128,
		Bucket: "test", ObjectKey: "staging/test", StorageUploadID: "fake-upload-id", PartSize: 128, PartCount: 1,
		Status: StatusUploading, IdempotencyKey: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour), Version: 1,
	}
	if err := fixture.db.Create(&session).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fixture.db.Create(&Part{SessionID: session.ID, PartNumber: 1, ETag: "etag", SizeBytes: 128, CompletedAt: time.Now()}).Error; err != nil {
		t.Fatalf("create part: %v", err)
	}

	const requests = 8
	errs := make(chan error, requests)
	var wait sync.WaitGroup
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := fixture.service.Complete(context.Background(), fixture.userID, session.ID)
			if err == nil && result.Status != StatusVerifying {
				err = ErrConflict
			}
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("complete upload: %v", err)
		}
	}
	_, completeCalls := fixture.storage.calls()
	if completeCalls != 1 {
		t.Fatalf("multipart completions = %d, want 1", completeCalls)
	}
	var eventCount int64
	if err := fixture.db.Model(&struct{ ID uuid.UUID }{}).Table("outbox_events").Where("aggregate_id = ? AND event_type = ?", session.ID, outbox.EventObjectVerifyRequested).Count(&eventCount).Error; err != nil || eventCount != 1 {
		t.Fatalf("verification events = %d, err = %v", eventCount, err)
	}
}

func TestAbortReleasesQuotaOnceUnderConcurrency(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	session := testActiveSession(fixture, time.Now().Add(time.Hour))
	if err := fixture.db.Create(&session).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fixture.db.Model(&identity.User{}).Where("id = ?", fixture.userID).Update("reserved_bytes", 256).Error; err != nil {
		t.Fatalf("reserve quota: %v", err)
	}

	const requests = 8
	errs := make(chan error, requests)
	var wait sync.WaitGroup
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- fixture.service.Abort(context.Background(), fixture.userID, session.ID)
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("abort upload: %v", err)
		}
	}

	var user identity.User
	if err := fixture.db.First(&user, "id = ?", fixture.userID).Error; err != nil || user.ReservedBytes != 128 {
		t.Fatalf("reserved bytes = %d, err = %v", user.ReservedBytes, err)
	}
	if fixture.storage.abortCount() != 1 {
		t.Fatalf("multipart aborts = %d, want 1", fixture.storage.abortCount())
	}
}

func TestExpireReleasesQuotaAndMarksSession(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	session := testActiveSession(fixture, time.Now().Add(-time.Minute))
	if err := fixture.db.Create(&session).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fixture.db.Model(&identity.User{}).Where("id = ?", fixture.userID).Update("reserved_bytes", session.ReservedBytes).Error; err != nil {
		t.Fatalf("reserve quota: %v", err)
	}

	count, err := fixture.service.Expire(context.Background(), 10)
	if err != nil || count != 1 {
		t.Fatalf("expire count = %d, err = %v", count, err)
	}
	stored, err := fixture.service.Get(fixture.userID, session.ID)
	if err != nil || stored.Status != StatusExpired || stored.ReservedBytes != 0 || stored.StorageUploadID != "" {
		t.Fatalf("expired session = %#v, err = %v", stored, err)
	}
	var user identity.User
	if err := fixture.db.First(&user, "id = ?", fixture.userID).Error; err != nil || user.ReservedBytes != 0 {
		t.Fatalf("reserved bytes = %d, err = %v", user.ReservedBytes, err)
	}
}

func TestVerifyRemovesCanonicalObjectWhenTransactionFails(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	storage := newFakeUploadStorage()
	fixture.service.storage = storage
	fixture.service.quota = commitFailQuota{Quota: fixture.service.quota, err: errors.New("forced quota failure")}

	content := []byte("copy succeeds but transaction fails")
	session := newVerifyingSession(fixture.userID, fixture.rootID, "compensation.txt", content)
	if err := fixture.db.Create(&session).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	storage.put(session.Bucket, session.ObjectKey, content)
	canonicalKey := canonicalObjectKey(session.DeclaredSHA256)

	err := fixture.service.Verify(context.Background(), session.ID)
	if err == nil || err.Error() != "forced quota failure" {
		t.Fatalf("verify error = %v", err)
	}
	if storage.exists(session.Bucket, canonicalKey) {
		t.Fatal("canonical object remains after transaction rollback")
	}
	if !storage.exists(session.Bucket, session.ObjectKey) {
		t.Fatal("retryable verification failure removed the staging object")
	}
	var objectCount int64
	if err := fixture.db.Model(&objectstore.Object{}).
		Where("sha256 = ? AND size_bytes = ?", session.DeclaredSHA256, session.SizeBytes).
		Count(&objectCount).Error; err != nil || objectCount != 0 {
		t.Fatalf("object records = %d, err = %v", objectCount, err)
	}
}

func TestVerifyHashMismatchRemovesStagingBeforeFailingSession(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	storage := newFakeUploadStorage()
	fixture.service.storage = storage

	content := []byte("actual content")
	session := newVerifyingSession(fixture.userID, fixture.rootID, "mismatch.txt", content)
	different := sha256.Sum256([]byte("different content"))
	session.DeclaredSHA256 = hex.EncodeToString(different[:])
	if err := fixture.db.Create(&session).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fixture.db.Model(&identity.User{}).Where("id = ?", fixture.userID).
		Update("reserved_bytes", session.ReservedBytes).Error; err != nil {
		t.Fatalf("reserve quota: %v", err)
	}
	storage.put(session.Bucket, session.ObjectKey, content)

	if err := fixture.service.Verify(context.Background(), session.ID); err != nil {
		t.Fatalf("verify mismatch: %v", err)
	}
	if storage.exists(session.Bucket, session.ObjectKey) {
		t.Fatal("hash mismatch left the staging object behind")
	}
	stored, err := fixture.service.Get(fixture.userID, session.ID)
	if err != nil || stored.Status != StatusFailed || stored.ErrorCode == nil || *stored.ErrorCode != "upload.hash_mismatch" {
		t.Fatalf("failed session = %#v, err = %v", stored, err)
	}
	var user identity.User
	if err := fixture.db.First(&user, "id = ?", fixture.userID).Error; err != nil || user.ReservedBytes != 0 {
		t.Fatalf("reserved bytes = %d, err = %v", user.ReservedBytes, err)
	}
}

func TestVerifyConcurrentSameHashKeepsCommittedCanonicalObject(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	storage := newFakeUploadStorage()
	fixture.service.storage = storage

	content := []byte("same content verified concurrently " + uuid.NewString())
	first := newVerifyingSession(fixture.userID, fixture.rootID, "same-a.txt", content)
	second := newVerifyingSession(fixture.userID, fixture.rootID, "same-b.txt", content)
	if err := fixture.db.Create(&first).Error; err != nil {
		t.Fatalf("create first session: %v", err)
	}
	if err := fixture.db.Create(&second).Error; err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if err := fixture.db.Model(&identity.User{}).Where("id = ?", fixture.userID).
		Update("reserved_bytes", first.ReservedBytes+second.ReservedBytes).Error; err != nil {
		t.Fatalf("reserve quota: %v", err)
	}
	storage.put(first.Bucket, first.ObjectKey, content)
	storage.put(second.Bucket, second.ObjectKey, content)

	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, sessionID := range []uuid.UUID{first.ID, second.ID} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- fixture.service.Verify(context.Background(), sessionID)
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("verify same hash: %v", err)
		}
	}

	canonicalKey := canonicalObjectKey(first.DeclaredSHA256)
	if !storage.exists(first.Bucket, canonicalKey) {
		t.Fatal("committed canonical object was removed by the losing verification")
	}
	if storage.exists(first.Bucket, first.ObjectKey) || storage.exists(second.Bucket, second.ObjectKey) {
		t.Fatal("completed concurrent verification left a staging object behind")
	}
	var sessions []Session
	if err := fixture.db.Where("id IN ?", []uuid.UUID{first.ID, second.ID}).Find(&sessions).Error; err != nil {
		t.Fatalf("load sessions: %v", err)
	}
	statuses := map[string]int{}
	for _, session := range sessions {
		statuses[session.Status]++
	}
	if statuses[StatusCompleted] != 1 || statuses[StatusFailed] != 1 {
		t.Fatalf("session statuses = %v", statuses)
	}
	var object objectstore.Object
	if err := fixture.db.Where("sha256 = ? AND size_bytes = ?", first.DeclaredSHA256, first.SizeBytes).First(&object).Error; err != nil {
		t.Fatalf("load canonical object: %v", err)
	}
	if object.Status != "ready" || object.ReferenceCount != 1 {
		t.Fatalf("canonical object = %#v", object)
	}
	var fileCount int64
	if err := fixture.db.Model(&drive.FileEntry{}).Where("owner_id = ? AND object_id = ? AND deleted_at IS NULL", fixture.userID, object.ID).Count(&fileCount).Error; err != nil || fileCount != 1 {
		t.Fatalf("file entries = %d, err = %v", fileCount, err)
	}
	t.Cleanup(func() {
		_ = fixture.db.Exec("DELETE FROM users WHERE id = ?", fixture.userID).Error
		_ = fixture.db.Exec("DELETE FROM outbox_events WHERE aggregate_id = ?", object.ID).Error
		_ = fixture.db.Exec("DELETE FROM file_objects WHERE id = ?", object.ID).Error
	})
}

func newVerifyingSession(userID, folderID uuid.UUID, filename string, content []byte) Session {
	digest := sha256.Sum256(content)
	return Session{
		ID: uuid.New(), UserID: userID, FolderID: folderID, Filename: filename, FilenameNormalized: filename,
		DeclaredSHA256: hex.EncodeToString(digest[:]), MimeType: "text/plain", SizeBytes: int64(len(content)), ReservedBytes: int64(len(content)),
		Bucket: "test", ObjectKey: "staging/" + uuid.NewString(), StorageUploadID: "completed-upload", PartSize: int64(len(content)), PartCount: 1,
		Status: StatusVerifying, IdempotencyKey: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour), Version: 1,
	}
}

func canonicalObjectKey(hash string) string {
	return "objects/sha256/" + hash[:2] + "/" + hash
}

func testActiveSession(fixture concurrencyFixture, expiresAt time.Time) Session {
	digest := sha256.Sum256([]byte(uuid.NewString()))
	return Session{
		ID: uuid.New(), UserID: fixture.userID, FolderID: fixture.rootID, Filename: "abort.txt", FilenameNormalized: "abort.txt",
		DeclaredSHA256: hex.EncodeToString(digest[:]), MimeType: "text/plain", SizeBytes: 128, ReservedBytes: 128,
		Bucket: "test", ObjectKey: "staging/abort", StorageUploadID: "fake-upload-id", PartSize: 128, PartCount: 1,
		Status: StatusUploading, IdempotencyKey: uuid.NewString(), ExpiresAt: expiresAt, Version: 1,
	}
}
