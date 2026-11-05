package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Stop after the actual provider has accepted bytes but before the application
// can publish metadata. No sleeps are needed to hit the GC/commit window.
type blockedVariantResponse struct {
	base    http.RoundTripper
	entered chan string
	release chan struct{}
	once    sync.Once
}

func (b *blockedVariantResponse) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := b.base.RoundTrip(req)
	if err == nil && req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/variants/") {
		b.once.Do(func() {
			b.entered <- strings.SplitN(strings.TrimPrefix(req.URL.Path, "/"), "/", 2)[1]
			select {
			case <-b.release:
			case <-req.Context().Done():
			}
		})
	}
	return res, err
}

func TestThumbnailWriteCleanupRaces(t *testing.T) {
	dsn, endpoint := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("TEST_MINIO_ENDPOINT")
	if dsn == "" || endpoint == "" {
		t.Skip("isolated PostgreSQL and MinIO required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	newClient := func(transport http.RoundTripper) *minio.Client {
		t.Helper()
		client, err := minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(os.Getenv("TEST_MINIO_ACCESS_KEY"), os.Getenv("TEST_MINIO_SECRET_KEY"), ""),
			Region: "us-east-1", Transport: transport,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	client := newClient(nil)
	for _, scenario := range []string{"gc_during_write", "concurrent_winner", "reclaimed_before_commit"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			bucket := "variant-" + uuid.NewString()
			if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			var content bytes.Buffer
			if err := png.Encode(&content, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
				t.Fatal(err)
			}
			id := uuid.New()
			digest := sha256.Sum256([]byte(id.String()))
			object := objectstore.Object{ID: id, SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(content.Len()), MimeType: "image/png", Bucket: bucket, ObjectKey: "source/" + id.String(), Status: "ready", Version: 1}
			if err := db.Create(&object).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Delete(&objectstore.Object{}, "id = ?", id) })
			if _, err := client.PutObject(ctx, bucket, object.ObjectKey, bytes.NewReader(content.Bytes()), int64(content.Len()), minio.PutObjectOptions{}); err != nil {
				t.Fatal(err)
			}
			objects := objectstore.NewService(db)
			blocked := &blockedVariantResponse{base: http.DefaultTransport, entered: make(chan string, 1), release: make(chan struct{})}
			var release sync.Once
			unblock := func() { release.Do(func() { close(blocked.release) }) }
			defer unblock()
			result := make(chan error, 1)
			go func() {
				result <- NewService(db, objects, newClient(blocked), nil, bucket, time.Minute).Process(ctx, id)
			}()
			var abandonedKey string
			select {
			case abandonedKey = <-blocked.entered:
			case err := <-result:
				t.Fatalf("writer returned before provider boundary: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var cleanup storageops.Operation
			if err := db.Where("object_id = ? AND operation_type = ?", id, storageops.VariantDelete).First(&cleanup).Error; err != nil {
				t.Fatal(err)
			}
			var winner Variant
			switch scenario {
			case "gc_during_write":
				if err := objects.GarbageCollect(ctx, client, id); err == nil {
					t.Fatal("GC must defer while thumbnail writer owns the lease")
				}
				if _, err := client.StatObject(ctx, bucket, object.ObjectKey, minio.StatObjectOptions{}); err != nil {
					t.Fatalf("GC removed source during live write: %v", err)
				}
			case "concurrent_winner":
				if err := NewService(db, objects, client, nil, bucket, time.Minute).Process(ctx, id); err != nil {
					t.Fatal(err)
				}
				if err := db.Where("object_id = ?", id).First(&winner).Error; err != nil {
					t.Fatal(err)
				}
				if winner.ObjectKey == abandonedKey {
					t.Fatal("concurrent writes reused the same provider key")
				}
			case "reclaimed_before_commit":
				if err := db.Model(&cleanup).Update("lease_until", time.Now().Add(-time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
				if n, err := objects.ReconcileObjectDeletes(ctx, client, 10); err != nil || n != 1 {
					t.Fatalf("reclaim abandoned write: n=%d err=%v", n, err)
				}
			}
			unblock()
			select {
			case err := <-result:
				if (err != nil) != (scenario == "reclaimed_before_commit") {
					t.Fatalf("writer error = %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := db.Model(&storageops.Operation{}).Where("object_id = ? AND state = ?", id, storageops.StateFailed).Update("available_at", time.Now().Add(-time.Second)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := objects.ReconcileObjectDeletes(ctx, client, 10); err != nil {
				t.Fatal(err)
			}
			if _, err := client.StatObject(ctx, bucket, abandonedKey, minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
				t.Fatalf("uncommitted thumbnail remains: %v", err)
			}
			var variants int64
			if err := db.Model(&Variant{}).Where("object_id = ?", id).Count(&variants).Error; err != nil {
				t.Fatal(err)
			}
			if scenario == "concurrent_winner" {
				if variants != 1 {
					t.Fatalf("committed variants = %d", variants)
				}
				if _, err := client.StatObject(ctx, bucket, winner.ObjectKey, minio.StatObjectOptions{}); err != nil {
					t.Fatalf("cleanup removed winner: %v", err)
				}
			} else if variants != 0 {
				t.Fatalf("retired or fenced writer committed %d variants", variants)
			}
			if scenario == "gc_during_write" {
				var retired objectstore.Object
				if err := db.First(&retired, "id = ?", id).Error; err != nil || retired.Status != "deleted" {
					t.Fatalf("GC did not converge: status=%s err=%v", retired.Status, err)
				}
			} else if scenario == "reclaimed_before_commit" {
				if err := NewService(db, objects, client, nil, bucket, time.Minute).Process(ctx, id); err != nil {
					t.Fatalf("replacement thumbnail could not finish: %v", err)
				}
			}
		})
	}
}
