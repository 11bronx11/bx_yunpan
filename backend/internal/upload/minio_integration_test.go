package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

type faultMinioTransport struct {
	base  http.RoundTripper
	down  atomic.Bool
	calls atomic.Int64
}

func (f *faultMinioTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	if f.down.Load() {
		return nil, errors.New("injected MinIO transport outage")
	}
	return f.base.RoundTrip(r)
}

func TestRealMinioUploadRecoversAfterCompleteDatabaseFailure(t *testing.T) {
	endpoint := os.Getenv("TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_MINIO_ENDPOINT is not set")
	}
	f := newConcurrencyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(base.CloseIdleConnections)
	transport := &faultMinioTransport{base: base}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("TEST_MINIO_ACCESS_KEY"), os.Getenv("TEST_MINIO_SECRET_KEY"), ""),
		Region: "us-east-1", Transport: transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "hc-" + uuid.NewString()
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	// Unique fixture objects/bucket are retained for inspection with the isolated volume.
	f.service.bucket = bucket
	f.service.core = minio.Core{Client: client}
	f.service.storage = minioUploadStorage{client: client}
	reader := drive.NewServiceWithCache(f.db, platformcache.NewJSONStore(nil, time.Minute, 8))
	if _, _, err := reader.Children(f.userID, f.rootID); err != nil {
		t.Fatal(err)
	}
	content := []byte("hc real multipart " + uuid.NewString())
	digest := sha256.Sum256(content)
	transport.down.Store(true)
	before := transport.calls.Load()
	result, err := f.service.Create(ctx, f.userID, CreateInput{
		FolderID: f.rootID, Filename: "real.txt", SHA256: hex.EncodeToString(digest[:]),
		SizeBytes: int64(len(content)), MimeType: "text/plain", IdempotencyKey: uuid.NewString(),
	})
	if err != nil || result.Upload == nil || result.Upload.Status != StatusInitializing || transport.calls.Load() != before {
		t.Fatalf("create while MinIO unavailable: result=%+v calls=%d error=%v", result, transport.calls.Load()-before, err)
	}
	session := *result.Upload
	t.Cleanup(func() {
		_ = f.db.Exec(`DELETE FROM outbox_events WHERE aggregate_id IN (
			SELECT id FROM upload_sessions WHERE user_id = ? UNION
			SELECT id FROM file_entries WHERE owner_id = ? UNION
			SELECT completed_object_id FROM upload_sessions WHERE user_id = ?)`, f.userID, f.userID, f.userID).Error
	})
	var create storageops.Operation
	if err := f.db.Where("upload_session_id = ? AND operation_type = ?", session.ID, storageops.MultipartCreate).First(&create).Error; err != nil {
		t.Fatal(err)
	}
	transport.down.Store(false)
	// Simulate provider success followed by a crash before persisting its ID.
	core := minio.Core{Client: client}
	orphanID, err := core.NewMultipartUpload(ctx, bucket, session.ObjectKey, minio.PutObjectOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileStorageOperation(ctx, create.ID); err != nil {
		t.Fatal(err)
	}
	session, err = f.service.Get(f.userID, session.ID)
	if err != nil || session.Status != StatusCreated || session.StorageUploadID == "" {
		t.Fatalf("reconciled create: session=%+v error=%v", session, err)
	}
	if session.StorageUploadID != orphanID {
		t.Fatalf("recovery created a new provider upload: got=%s want=%s", session.StorageUploadID, orphanID)
	}
	part, err := core.PutObjectPart(ctx, bucket, session.ObjectKey, session.StorageUploadID, 1, bytes.NewReader(content), int64(len(content)), minio.PutObjectPartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ConfirmParts(f.userID, session.ID, []ConfirmedPart{{PartNumber: 1, ETag: part.ETag, SizeBytes: int64(len(content))}}); err != nil {
		t.Fatal(err)
	}
	transport.down.Store(true)
	before = transport.calls.Load()
	if got, err := f.service.Complete(ctx, f.userID, session.ID); err != nil || got.Status != StatusCompleting || transport.calls.Load() != before {
		t.Fatalf("complete while MinIO unavailable: status=%s calls=%d error=%v", got.Status, transport.calls.Load()-before, err)
	}
	transport.down.Store(false)
	var complete storageops.Operation
	if err := f.db.Where("upload_session_id = ? AND operation_type = ?", session.ID, storageops.MultipartComplete).First(&complete).Error; err != nil {
		t.Fatal(err)
	}
	trigger := "hc_fail_" + strings.ReplaceAll(session.ID.String(), "-", "")
	if err := f.db.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.id = '%s'::uuid AND NEW.status = 'verifying' THEN RAISE EXCEPTION 'hc injected state failure'; END IF;
		RETURN NEW; END $$`, trigger, session.ID)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Exec("DROP FUNCTION " + trigger + "() CASCADE").Error })
	if err := f.db.Exec(fmt.Sprintf("CREATE TRIGGER %s BEFORE UPDATE ON upload_sessions FOR EACH ROW EXECUTE FUNCTION %s()", trigger, trigger)).Error; err != nil {
		t.Fatal(err)
	}
	err = f.service.ReconcileStorageOperation(ctx, complete.ID)
	if err == nil || !strings.Contains(err.Error(), "hc injected state failure") {
		t.Fatalf("expected database failure after provider success, got %v", err)
	}
	if info, err := client.StatObject(ctx, bucket, session.ObjectKey, minio.StatObjectOptions{}); err != nil || info.Size != int64(len(content)) {
		t.Fatalf("provider completion not durable: info=%+v error=%v", info, err)
	}
	if err := f.db.Exec("DROP TRIGGER " + trigger + " ON upload_sessions").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.service.ReconcileStorageOperation(ctx, complete.ID); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := f.db.Table("outbox_events").Where("aggregate_id = ? AND event_type = ?", session.ID, outbox.EventObjectVerifyRequested).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("verify events=%d error=%v", count, err)
	}
	for range 2 {
		if err := f.service.Verify(ctx, session.ID); err != nil {
			t.Fatal(err)
		}
	}
	session, err = f.service.Get(f.userID, session.ID)
	if err != nil || session.Status != StatusCompleted || session.CompletedEntryID == nil || session.CompletedObjectID == nil {
		t.Fatalf("final upload state=%+v error=%v", session, err)
	}
	_, files, err := reader.Children(f.userID, f.rootID)
	if err != nil || len(files) != 1 || files[0].ID != *session.CompletedEntryID {
		t.Fatalf("worker commit left stale API cache: files=%v error=%v", files, err)
	}
	var user identity.User
	if err := f.db.First(&user, "id = ?", f.userID).Error; err != nil || user.ReservedBytes != 0 || user.UsedLogicalBytes != int64(len(content)) {
		t.Fatalf("quota after duplicate verification: user=%+v error=%v", user, err)
	}
	object, err := f.service.objects.Get(*session.CompletedObjectID)
	if err != nil || object.ReferenceCount != 1 {
		t.Fatalf("final object=%+v error=%v", object, err)
	}
	stored, err := client.GetObject(ctx, bucket, object.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	actual, err := io.ReadAll(stored)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("canonical content mismatch: error=%v", err)
	}
	t.Log("real MinIO complete -> PostgreSQL state failure -> recovery -> single entry/reference/quota commit verified")
}
