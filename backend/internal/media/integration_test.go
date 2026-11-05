package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestThumbnailPermanentInputAndTransientStorageFailures(t *testing.T) {
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
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("TEST_MINIO_ACCESS_KEY"), os.Getenv("TEST_MINIO_SECRET_KEY"), ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bucket := "media-" + uuid.NewString()
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	service := NewService(db, objectstore.NewService(db), client, nil, bucket, time.Minute)
	var valid bytes.Buffer
	if err := png.Encode(&valid, image.NewRGBA(image.Rect(0, 0, 10, 10))); err != nil {
		t.Fatal(err)
	}
	// A valid PNG header advertises excessive pixels without allocating them.
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header[:4], 10000)
	binary.BigEndian.PutUint32(header[4:8], 5000)
	header[8] = 8
	header[9] = 2
	var huge bytes.Buffer
	huge.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&huge, binary.BigEndian, uint32(len(header)))
	huge.WriteString("IHDR")
	huge.Write(header)
	_ = binary.Write(&huge, binary.BigEndian, crc32.ChecksumIEEE(append([]byte("IHDR"), header...)))
	for _, test := range []struct {
		name              string
		content           []byte
		missing, terminal bool
	}{
		{"unsupported", []byte("not an image"), false, true},
		{"excessive_dimensions", huge.Bytes(), false, true},
		{"storage_missing", nil, true, false},
		{"valid", valid.Bytes(), false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := uuid.New()
			digest := sha256.Sum256([]byte(id.String()))
			object := objectstore.Object{ID: id, SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(test.content)) + 1, MimeType: "image/png", Bucket: bucket, ObjectKey: "source/" + id.String(), Status: "ready", ReferenceCount: 1, Version: 1}
			if err := db.Create(&object).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Delete(&objectstore.Object{}, "id = ?", id) })
			if !test.missing {
				if _, err := client.PutObject(ctx, bucket, object.ObjectKey, bytes.NewReader(test.content), int64(len(test.content)), minio.PutObjectOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			err := service.Process(ctx, id)
			if errors.Is(err, taskqueue.SkipRetry) != test.terminal {
				t.Fatalf("terminal=%v want=%v error=%v", errors.Is(err, taskqueue.SkipRetry), test.terminal, err)
			}
			if test.missing && err == nil {
				t.Fatal("storage failure acknowledged as success")
			}
			if test.name == "valid" && err != nil {
				t.Fatal(err)
			}
		})
	}
}
