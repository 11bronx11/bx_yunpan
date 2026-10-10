package upload

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

func TestCanonicalLegacyOrphanEntersGC(t *testing.T) {
	f := newConcurrencyFixture(t)
	store := newFakeUploadStorage()
	f.service.storage = store
	content := []byte("legacy orphan " + uuid.NewString())
	session := newVerifyingSession(f.userID, f.rootID, "orphan.txt", content)
	if err := f.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	store.put(session.Bucket, session.ObjectKey, content)
	ctx := context.Background()
	key, err := f.service.ensureCanonical(ctx, session, session.DeclaredSHA256, session.SizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	// This is the old release's durable failure state: bytes but no object row.
	if err := f.db.Model(&session).Updates(map[string]any{"status": StatusFailed, "reserved_bytes": 0}).Error; err != nil {
		t.Fatal(err)
	}
	count, err := f.service.reconcileCanonicalOrphans(ctx, 100)
	if err != nil || count < 1 {
		t.Fatalf("reconcile count=%d err=%v", count, err)
	}
	var object objectstore.Object
	if err := f.db.Where("sha256 = ?", session.DeclaredSHA256).First(&object).Error; err != nil {
		t.Fatal(err)
	}
	if object.ObjectKey != key || object.ReferenceCount != 0 {
		t.Fatalf("adopted object=%+v", object)
	}
	if err := f.service.objects.GarbageCollect(ctx, store, object.ID); err != nil {
		t.Fatal(err)
	}
	if store.exists(object.Bucket, object.ObjectKey) {
		t.Fatal("orphan bytes retained")
	}
	if count, err := f.service.reconcileCanonicalOrphans(ctx, 100); err != nil || count != 0 {
		t.Fatalf("repeat count=%d error=%v", count, err)
	}
}

func TestRealMinioCanonicalFailureCleanup(t *testing.T) {
	if os.Getenv("TEST_MINIO_ENDPOINT") == "" {
		t.Skip("TEST_MINIO_ENDPOINT is required")
	}
	f := newConcurrencyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := minio.New(os.Getenv("TEST_MINIO_ENDPOINT"), &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("TEST_MINIO_ACCESS_KEY"), os.Getenv("TEST_MINIO_SECRET_KEY"), "")})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "canonical-" + uuid.NewString()
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	f.service.storage = minioUploadStorage{client: client}
	prepare := func(name string, content []byte) Session {
		t.Helper()
		s := newVerifyingSession(f.userID, f.rootID, name, content)
		s.Bucket = bucket
		if err := f.db.Create(&s).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.db.Model(&identity.User{}).Where("id = ?", f.userID).Update("reserved_bytes", s.ReservedBytes).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := client.PutObject(ctx, bucket, s.ObjectKey, bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := prepare("conflict.txt", []byte("keep "+uuid.NewString()))
	if err := f.service.Verify(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	failedBytes := []byte("collect " + uuid.NewString())
	second := prepare("conflict.txt", failedBytes)
	if err := f.service.Verify(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	failed, err := f.service.Get(f.userID, second.ID)
	if err != nil || failed.Status != StatusFailed {
		t.Fatalf("session=%+v error=%v", failed, err)
	}
	var orphan objectstore.Object
	if err := f.db.Where("sha256 = ?", second.DeclaredSHA256).First(&orphan).Error; err != nil {
		t.Fatal(err)
	}
	if orphan.ReferenceCount != 0 {
		t.Fatalf("orphan references=%d", orphan.ReferenceCount)
	}
	if _, err := client.StatObject(ctx, bucket, orphan.ObjectKey, minio.StatObjectOptions{}); err != nil {
		t.Fatalf("failure did not reach post-copy window: %v", err)
	}
	if err := f.service.objects.GarbageCollect(ctx, f.service.storage, orphan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.StatObject(ctx, bucket, orphan.ObjectKey, minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
		t.Fatalf("orphan still present: %v", err)
	}
	firstKey := canonicalObjectKey(first.DeclaredSHA256)
	if _, err := client.StatObject(ctx, bucket, firstKey, minio.StatObjectOptions{}); err != nil {
		t.Fatalf("cleanup damaged existing file: %v", err)
	}
	third := prepare("retry.txt", failedBytes)
	if err := f.service.Verify(ctx, third.ID); err != nil {
		t.Fatal(err)
	}
	var revived objectstore.Object
	if err := f.db.First(&revived, "id = ?", orphan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if revived.ObjectKey == orphan.ObjectKey || revived.ReferenceCount != 1 {
		t.Fatalf("revival=%+v", revived)
	}
	// Scan the complete isolated bucket, not only the database reference counts.
	objects := 0
	for item := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if item.Err != nil {
			t.Fatal(item.Err)
		}
		var count int64
		if err := f.db.Model(&objectstore.Object{}).Where("bucket = ? AND object_key = ? AND status = 'ready'", bucket, item.Key).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("untracked object %s count=%d err=%v", item.Key, count, err)
		}
		objects++
	}
	if objects != 2 {
		t.Fatalf("bucket objects=%d, want two referenced objects", objects)
	}
	t.Logf("full isolated bucket scan: objects=%d untracked=0", objects)
}

func TestFailedSessionDoesNotRestartCanonicalCopy(t *testing.T) {
	f := newConcurrencyFixture(t)
	store := newFakeUploadStorage()
	f.service.storage = store
	content := []byte("terminal copy " + uuid.NewString())
	session := newVerifyingSession(f.userID, f.rootID, "terminal.txt", content)
	if err := f.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	store.put(session.Bucket, session.ObjectKey, content)
	operation, err := storageops.Enqueue(f.db, storageops.Input{OperationType: storageops.CanonicalEnsure, IdempotencyKey: uuid.NewString(), UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey, TargetBucket: session.Bucket, TargetKey: canonicalObjectKey(session.DeclaredSHA256)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&session).Update("status", StatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileStorageOperation(context.Background(), operation.ID); err != nil {
		t.Fatal(err)
	}
	if store.exists(session.Bucket, operation.TargetKey) {
		t.Fatal("terminal session copied bytes again")
	}
	_, err = f.service.ensureCanonical(context.Background(), session, session.DeclaredSHA256, session.SizeBytes)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal ensure error=%v", err)
	}
}
