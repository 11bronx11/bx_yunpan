package drive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type moveFixture struct {
	service *Service
	db      *gorm.DB
	ownerID uuid.UUID
	source  Folder
	target  Folder
	object  uuid.UUID
	file    FileEntry
}

func newMoveFixture(t *testing.T) moveFixture {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })

	ownerID := uuid.New()
	username := "move-" + ownerID.String()
	if err := tx.Exec(`
		INSERT INTO users (id, username, username_normalized, password_hash)
		VALUES (?, ?, ?, ?)`, ownerID, username, username, "test").Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	service := NewService(tx)
	if err := service.ProvisionUser(tx, ownerID); err != nil {
		t.Fatalf("provision root: %v", err)
	}
	root, err := service.Root(ownerID)
	if err != nil {
		t.Fatalf("load root: %v", err)
	}
	source, err := service.Create(ownerID, root.ID, "source")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	target, err := service.Create(ownerID, root.ID, "target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	objectID := createMoveObject(t, tx)
	file, err := service.CreateFile(tx, ownerID, source.ID, objectID, "report.pdf")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	return moveFixture{service: service, db: tx, ownerID: ownerID, source: source, target: target, object: objectID, file: file}
}

func createMoveObject(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	objectID := uuid.New()
	digest := sha256.Sum256([]byte(objectID.String()))
	hash := hex.EncodeToString(digest[:])
	if err := db.Exec(`
		INSERT INTO file_objects (id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count)
		VALUES (?, ?, ?, ?, ?, ?, 'ready', 1)`, objectID, hash, 1024, "application/pdf", "test", "objects/"+hash).Error; err != nil {
		t.Fatalf("create object: %v", err)
	}
	return objectID
}

func TestMoveFileChangesOnlyDirectoryMetadata(t *testing.T) {
	fixture := newMoveFixture(t)

	moved, err := fixture.service.MoveFile(fixture.ownerID, fixture.file.ID, fixture.target.ID, fixture.file.Version)
	if err != nil {
		t.Fatalf("move file: %v", err)
	}
	if moved.FolderID != fixture.target.ID {
		t.Fatalf("folder ID = %s, want %s", moved.FolderID, fixture.target.ID)
	}
	if moved.ObjectID != fixture.object {
		t.Fatalf("object ID changed to %s, want %s", moved.ObjectID, fixture.object)
	}
	if moved.Version != fixture.file.Version+1 {
		t.Fatalf("version = %d, want %d", moved.Version, fixture.file.Version+1)
	}
	if moved.MimeType != "application/pdf" || moved.SizeBytes != 1024 {
		t.Fatalf("content metadata changed: mime=%s size=%d", moved.MimeType, moved.SizeBytes)
	}
}

func TestFileViewsIncludeAIStatus(t *testing.T) {
	fixture := newMoveFixture(t)
	documentID := uuid.New()
	if err := fixture.db.Exec(`
		INSERT INTO ai_documents (id, object_id, status, pipeline_version, model_version)
		VALUES (?, ?, 'failed', 'document-index-v2', 'test-model')`, documentID, fixture.object).Error; err != nil {
		t.Fatalf("create AI document: %v", err)
	}

	file, err := fixture.service.File(fixture.ownerID, fixture.file.ID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	if file.AIStatus == nil || *file.AIStatus != "failed" {
		t.Fatalf("file AI status = %v, want failed", file.AIStatus)
	}

	_, files, err := fixture.service.Children(fixture.ownerID, fixture.source.ID)
	if err != nil {
		t.Fatalf("load children: %v", err)
	}
	if len(files) != 1 || files[0].AIStatus == nil || *files[0].AIStatus != "failed" {
		t.Fatalf("children AI status = %+v, want failed", files)
	}
}

func TestMoveFileRejectsDuplicateName(t *testing.T) {
	fixture := newMoveFixture(t)
	otherObject := createMoveObject(t, fixture.db)
	if _, err := fixture.service.CreateFile(fixture.db, fixture.ownerID, fixture.target.ID, otherObject, fixture.file.Name); err != nil {
		t.Fatalf("create conflicting file: %v", err)
	}

	_, err := fixture.service.MoveFile(fixture.ownerID, fixture.file.ID, fixture.target.ID, fixture.file.Version)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("move error = %v, want ErrConflict", err)
	}
}

func TestCreateFileRejectsDuplicateContentAcrossDirectories(t *testing.T) {
	fixture := newMoveFixture(t)

	_, err := fixture.service.CreateFile(fixture.db, fixture.ownerID, fixture.target.ID, fixture.object, "copy.pdf")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("create duplicate content error = %v, want ErrConflict", err)
	}
}

func TestMoveFileRejectsStaleVersionAndUnknownTarget(t *testing.T) {
	fixture := newMoveFixture(t)

	if _, err := fixture.service.MoveFile(fixture.ownerID, fixture.file.ID, fixture.target.ID, fixture.file.Version+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version error = %v, want ErrConflict", err)
	}
	if _, err := fixture.service.MoveFile(fixture.ownerID, fixture.file.ID, uuid.New(), fixture.file.Version); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target error = %v, want ErrNotFound", err)
	}
}
