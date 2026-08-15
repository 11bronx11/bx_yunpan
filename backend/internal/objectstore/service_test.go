package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type recordingGCStorage struct {
	mu      sync.Mutex
	removed []string
}

func (s *recordingGCStorage) RemoveObject(_ context.Context, bucket, object string, _ minio.RemoveObjectOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, bucket+"/"+object)
	return nil
}

func (s *recordingGCStorage) removedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.removed...)
}

func TestCreateOrGetReturnsExistingObject(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })

	service := NewService(db)
	digest := sha256.Sum256([]byte(uuid.NewString()))
	hash := hex.EncodeToString(digest[:])
	size := time.Now().UnixNano()
	original := Object{
		ID: uuid.New(), SHA256: hash, SizeBytes: size, MimeType: "application/octet-stream",
		Bucket: "test", ObjectKey: "objects/sha256/" + hash[:2] + "/" + hash, Status: "ready",
	}
	created, err := service.CreateOrGet(tx, original)
	if err != nil {
		t.Fatalf("create object: %v", err)
	}

	duplicate := original
	duplicate.ID = uuid.New()
	got, err := service.CreateOrGet(tx, duplicate)
	if err != nil {
		t.Fatalf("get existing object: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("object ID = %s, want existing ID %s", got.ID, created.ID)
	}
}

func TestGarbageCollectRemovesDerivedDataAndIsIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })

	objectID := uuid.New()
	documentID := uuid.New()
	digest := sha256.Sum256([]byte(uuid.NewString()))
	hash := hex.EncodeToString(digest[:])
	objectKey := "objects/sha256/" + hash[:2] + "/" + hash
	variantKey := "variants/" + objectID.String() + "/thumbnail/thumbnail-v1.jpg"
	object := Object{
		ID: objectID, SHA256: hash, SizeBytes: time.Now().UnixNano(), MimeType: "image/jpeg",
		Bucket: "test", ObjectKey: objectKey, Status: "ready",
	}
	if err := tx.Create(&object).Error; err != nil {
		t.Fatalf("create object: %v", err)
	}
	if err := tx.Exec(`
		INSERT INTO object_variants
			(id, object_id, variant_type, object_key, mime_type, width, height, pipeline_version, status)
		VALUES (?, ?, 'thumbnail', ?, 'image/jpeg', 128, 128, 'thumbnail-v1', 'ready')`,
		uuid.New(), objectID, variantKey).Error; err != nil {
		t.Fatalf("create variant: %v", err)
	}
	if err := tx.Exec(`
		INSERT INTO ai_documents (id, object_id, status, pipeline_version, model_version)
		VALUES (?, ?, 'indexed', 'document-index-v2', 'test-model')`, documentID, objectID).Error; err != nil {
		t.Fatalf("create AI document: %v", err)
	}
	if err := tx.Exec(`
		INSERT INTO ai_chunks (id, document_id, ordinal, content, embedding)
		VALUES (?, ?, 0, 'test chunk', array_fill(0::real, ARRAY[1024])::vector)`, uuid.New(), documentID).Error; err != nil {
		t.Fatalf("create AI chunk: %v", err)
	}

	storage := &recordingGCStorage{}
	service := NewService(tx)
	if err := service.GarbageCollect(context.Background(), storage, objectID); err != nil {
		t.Fatalf("garbage collect: %v", err)
	}
	if got := storage.removedKeys(); len(got) != 2 || got[0] != "test/"+variantKey || got[1] != "test/"+objectKey {
		t.Fatalf("removed keys = %v", got)
	}

	var stored Object
	if err := tx.First(&stored, "id = ?", objectID).Error; err != nil {
		t.Fatalf("load collected object: %v", err)
	}
	if stored.Status != "deleted" || stored.DeletedAt == nil {
		t.Fatalf("collected object = %#v", stored)
	}
	for table, condition := range map[string]string{
		"object_variants": "object_id = ?",
		"ai_documents":    "object_id = ?",
		"ai_chunks":       "document_id = ?",
	} {
		id := objectID
		if table == "ai_chunks" {
			id = documentID
		}
		var count int64
		if err := tx.Table(table).Where(condition, id).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("%s rows = %d, err = %v", table, count, err)
		}
	}

	if err := service.GarbageCollect(context.Background(), storage, objectID); err != nil {
		t.Fatalf("retry garbage collect: %v", err)
	}
	if got := storage.removedKeys(); len(got) != 2 {
		t.Fatalf("retry removed keys = %v", got)
	}
}
