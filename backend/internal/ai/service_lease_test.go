package ai

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestDocumentProcessingLease(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	objectID := uuid.New()
	sha := strings.Repeat("a", 32) + strings.ReplaceAll(objectID.String(), "-", "")
	if err := db.Exec(`INSERT INTO file_objects
		(id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count, created_at, updated_at)
		VALUES (?, ?, 1, 'text/plain', 'test', ?, 'ready', 0, now(), now())`, objectID, sha, "lease/"+objectID.String()).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DELETE FROM file_objects WHERE id = ?", objectID)

	service := &Service{db: db, provider: &fakeProvider{dimension: 1024}}
	document, acquired, err := service.startDocument(objectID, false)
	if err != nil || !acquired {
		t.Fatalf("first worker did not acquire lease: acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err = service.startDocument(objectID, false); !errors.Is(err, ErrProcessing) || acquired {
		t.Fatalf("duplicate event did not report active lease: acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err = service.startDocument(objectID, true); !errors.Is(err, ErrProcessing) || acquired {
		t.Fatalf("forced reprocess did not report active lease: acquired=%v err=%v", acquired, err)
	}
	if err := db.Model(&Document{}).Where("id = ?", document.ID).Update("status", "indexed").Error; err != nil {
		t.Fatal(err)
	}
	if _, acquired, err = service.startDocument(objectID, false); err != nil || acquired {
		t.Fatalf("current index was rebuilt by ordinary event: acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err = service.startDocument(objectID, true); err != nil || !acquired {
		t.Fatalf("explicit reprocess did not acquire lease: acquired=%v err=%v", acquired, err)
	}
}
