package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/recovery"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Exercise the actual role handlers, relay and periodic runners together.
// Every run has its own vhost, user and bucket; no existing queue is purged.
func TestAsyncUploadAndGCWithTwoMaintenanceScanners(t *testing.T) {
	dsn, endpoint, rabbit, management := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("TEST_MINIO_ENDPOINT"), os.Getenv("TEST_RABBITMQ_URL"), os.Getenv("TEST_RABBITMQ_MANAGEMENT_URL")
	if dsn == "" || endpoint == "" || rabbit == "" || management == "" {
		t.Skip("isolated PostgreSQL, RabbitMQ and MinIO required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("TEST_MINIO_ACCESS_KEY"), os.Getenv("TEST_MINIO_SECRET_KEY"), ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ctx = outbox.WithV2EventsEnabled(ctx, true)
	bucket := "async-" + uuid.NewString()
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(rabbit)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	vhost := "async-" + uuid.NewString()
	request := func(method, path string, body []byte) {
		t.Helper()
		req, err := http.NewRequest(method, management+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(u.User.Username(), password)
		req.Header.Set("Content-Type", "application/json")
		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			t.Fatalf("vhost request: %d", res.StatusCode)
		}
	}
	request("PUT", "/api/vhosts/"+vhost, []byte(`{}`))
	request("PUT", "/api/permissions/"+vhost+"/"+url.PathEscape(u.User.Username()), []byte(`{"configure":".*","write":".*","read":".*"}`))
	t.Cleanup(func() { request("DELETE", "/api/vhosts/"+vhost, nil) })
	u.Path = "/" + vhost
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	user := identity.User{ID: uuid.New(), Username: "async-" + uuid.NewString(), PasswordHash: "test", Status: 1, QuotaBytes: 1 << 30}
	user.UsernameNormalized = user.Username
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM users WHERE id = ?", user.ID) })
	driveService := drive.NewService(db)
	if err := driveService.ProvisionUser(db, user.ID); err != nil {
		t.Fatal(err)
	}
	root, err := driveService.Root(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	objects := objectstore.NewService(db)
	quota := identity.NewService(db, nil, driveService, 1<<30)
	uploads := upload.NewService(db, client, nil, bucket, driveService, objects, quota, config.Upload{SessionTTL: time.Hour, PartURLTTL: time.Minute})
	mediaService := media.NewService(db, objects, client, nil, bucket, time.Minute)
	var wg sync.WaitGroup
	run := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }
	var brokers []*taskqueue.Broker
	startConsumer := func(queues map[string]int, handlers map[string]taskqueue.HandlerFunc) {
		broker := taskqueue.New(taskqueue.Config{URL: u.String(), Concurrency: 4, Prefetch: 4, StopTimeout: time.Second})
		brokers = append(brokers, broker)
		broker.ExecutionStore = taskqueue.NewStore(db).WithStopTimeout(time.Second)
		broker.FailureRecorder = broker.ExecutionStore.RecordFailure
		run(func() {
			if err := broker.Consume(ctx, queues, handlers, logger); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		})
	}
	// Register joining before starting any goroutine, including early-failure paths.
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		for _, b := range brokers {
			_ = b.Close()
		}
	})
	startConsumer(map[string]int{"upload": 1}, map[string]taskqueue.HandlerFunc{outbox.TaskUploadOperationRecoverV1: uploadRecoveryHandler(uploads)})
	startConsumer(map[string]int{"object": 1}, map[string]taskqueue.HandlerFunc{outbox.TaskObjectVerifyV1: upload.VerifyHandler(uploads)})
	startConsumer(map[string]int{"media": 1}, map[string]taskqueue.HandlerFunc{outbox.TaskMediaThumbnailV1: media.Handler(mediaService), outbox.EventMediaThumbnailRequested: media.Handler(mediaService)})
	noop := func(context.Context, *taskqueue.Task) error { return nil }
	startConsumer(map[string]int{"maintenance": 1}, map[string]taskqueue.HandlerFunc{outbox.TaskObjectGCV1: objectstore.GCHandler(objects, client), outbox.EventStorageOperationRecover: maintenanceRecoveryHandler(objects, client, logger), outbox.EventFileCreated: noop})
	publisher := taskqueue.New(taskqueue.Config{URL: u.String()})
	brokers = append(brokers, publisher)
	dispatcher := outbox.NewDispatcher(db, publisher, logger, config.Outbox{PollInterval: 20 * time.Millisecond, MaxPollInterval: 50 * time.Millisecond, ScanTimeout: time.Second, BatchSize: 16, MaxRetry: 3, TaskTimeout: 10 * time.Second}).WithAIEnabled(false)
	run(func() { dispatcher.Run(ctx) })
	for range 2 {
		maintenance := upload.NewRecoveryService(db, objects, quota)
		run(func() { runUploadCleanup(ctx, maintenance, 100*time.Millisecond, 20, logger) })
		run(func() { runRecoveryReconciler(ctx, recovery.New(db, false), 100*time.Millisecond, 20, logger) })
		run(func() { runCanonicalOrphanReconciler(ctx, maintenance, 100*time.Millisecond, 20, logger) })
	}
	wait := func(label string, condition func() bool) {
		t.Helper()
		for !condition() {
			select {
			case <-ctx.Done():
				t.Fatalf("%s: %v", label, ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	content := []byte("complete async pipeline " + uuid.NewString())
	digest := sha256.Sum256(content)
	created, err := uploads.Create(ctx, user.ID, upload.CreateInput{FolderID: root.ID, Filename: "pipeline.txt", SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(content)), MimeType: "text/plain", IdempotencyKey: uuid.NewString()})
	if err != nil || created.Upload == nil {
		t.Fatalf("create: %v %+v", err, created)
	}
	sessionID := created.Upload.ID
	var session upload.Session
	wait("multipart create", func() bool {
		var e error
		session, e = uploads.Get(user.ID, sessionID)
		return e == nil && session.Status == upload.StatusCreated
	})
	core := minio.Core{Client: client}
	part, err := core.PutObjectPart(ctx, bucket, session.ObjectKey, session.StorageUploadID, 1, bytes.NewReader(content), int64(len(content)), minio.PutObjectPartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uploads.ConfirmParts(user.ID, sessionID, []upload.ConfirmedPart{{PartNumber: 1, ETag: part.ETag, SizeBytes: int64(len(content))}}); err != nil {
		t.Fatal(err)
	}
	if _, err := uploads.Complete(ctx, user.ID, sessionID); err != nil {
		t.Fatal(err)
	}
	wait("verify and commit", func() bool {
		var e error
		session, e = uploads.Get(user.ID, sessionID)
		return e == nil && session.Status == upload.StatusCompleted
	})
	object, err := objects.Get(*session.CompletedObjectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StatObject(ctx, bucket, object.ObjectKey, minio.StatObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	wait("staging delete", func() bool {
		_, err := client.StatObject(ctx, bucket, session.ObjectKey, minio.StatObjectOptions{})
		return minio.ToErrorResponse(err).Code == "NoSuchKey"
	})
	entry, err := driveService.File(user.ID, *session.CompletedEntryID)
	if err != nil {
		t.Fatal(err)
	}
	files := drive.NewFileManager(driveService, objects, quota, nil, nil, time.Minute)
	if err := files.Delete(ctx, user.ID, entry.ID, entry.Version); err != nil {
		t.Fatal(err)
	}
	wait("object GC", func() bool {
		var current objectstore.Object
		return db.First(&current, "id = ?", object.ID).Error == nil && current.Status == "deleted"
	})
	for item := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if item.Err != nil {
			t.Fatal(item.Err)
		}
		t.Fatalf("GC retained %s", item.Key)
	}
	// Expiry must release quota and abort through the upload queue.
	digest = sha256.Sum256([]byte(uuid.NewString()))
	expiring, err := uploads.Create(ctx, user.ID, upload.CreateInput{FolderID: root.ID, Filename: "expire.txt", SHA256: hex.EncodeToString(digest[:]), SizeBytes: 128, MimeType: "text/plain", IdempotencyKey: uuid.NewString()})
	if err != nil || expiring.Upload == nil {
		t.Fatal(err)
	}
	wait("expiring upload create", func() bool {
		s, e := uploads.Get(user.ID, expiring.Upload.ID)
		return e == nil && s.Status == upload.StatusCreated
	})
	if err := db.Model(&upload.Session{}).Where("id = ?", expiring.Upload.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	wait("expiry and abort", func() bool {
		s, e := uploads.Get(user.ID, expiring.Upload.ID)
		return e == nil && s.Status == upload.StatusExpired && s.StorageUploadID == ""
	})
	var stored identity.User
	if err := db.First(&stored, "id = ?", user.ID).Error; err != nil || stored.ReservedBytes != 0 || stored.UsedLogicalBytes != 0 {
		t.Fatalf("quota: %+v %v", stored, err)
	}
	var failures int64
	if err := db.Model(&taskqueue.Failure{}).Count(&failures).Error; err != nil || failures != 0 {
		t.Fatalf("pipeline failures=%d err=%v", failures, err)
	}
	t.Log(fmt.Sprintf("real RabbitMQ/MinIO: upload -> verify -> staging delete -> file delete -> GC; expiry -> abort; maintenance scanners=2, failures=%d", failures))
}
