package sharing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func TestImportSerializesConcurrentIdempotentRequests(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	driveService := drive.NewService(db)
	objects := objectstore.NewService(db)
	quota := identity.NewService(db, nil, driveService, 10*1024*1024*1024)
	service := NewService(db, driveService, objects, quota, config.Sharing{Secret: "concurrency-test", AccessTTL: time.Minute})

	ownerID, recipientID := uuid.New(), uuid.New()
	createUser := func(id uuid.UUID, prefix string) drive.Folder {
		username := prefix + "-" + id.String()
		if err := db.Exec(`INSERT INTO users (id, username, username_normalized, password_hash) VALUES (?, ?, ?, ?)`, id, username, username, "test").Error; err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := driveService.ProvisionUser(db, id); err != nil {
			t.Fatalf("provision root: %v", err)
		}
		root, err := driveService.Root(id)
		if err != nil {
			t.Fatalf("load root: %v", err)
		}
		return root
	}
	ownerRoot := createUser(ownerID, "share-owner")
	recipientRoot := createUser(recipientID, "share-recipient")
	digest := sha256.Sum256([]byte(uuid.NewString()))
	object := objectstore.Object{
		ID: uuid.New(), SHA256: hex.EncodeToString(digest[:]), SizeBytes: 256, MimeType: "text/plain",
		Bucket: "test", ObjectKey: "objects/" + uuid.NewString(), Status: "ready",
	}
	if err := db.Create(&object).Error; err != nil {
		t.Fatalf("create object: %v", err)
	}
	source, err := driveService.CreateFile(db, ownerID, ownerRoot.ID, object.ID, "shared.txt")
	if err != nil {
		t.Fatalf("create source file: %v", err)
	}
	if err := objects.AddReference(db, object.ID); err != nil {
		t.Fatalf("reference source object: %v", err)
	}
	share, key, err := service.Create(ownerID, source.ID, nil)
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	_, _, accessToken, _, err := service.Resolve(key)
	if err != nil {
		t.Fatalf("resolve share: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM outbox_events WHERE aggregate_id = ?", share.ID).Error
		_ = db.Exec("DELETE FROM share_imports WHERE share_id = ?", share.ID).Error
		_ = db.Exec("DELETE FROM shares WHERE id = ?", share.ID).Error
		_ = db.Exec("DELETE FROM users WHERE id IN ?", []uuid.UUID{ownerID, recipientID}).Error
		_ = db.Exec("DELETE FROM file_objects WHERE id = ?", object.ID).Error
	})

	const requests = 8
	results := make(chan drive.FileView, requests)
	errs := make(chan error, requests)
	var wait sync.WaitGroup
	idempotencyKey := uuid.NewString()
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			file, err := service.Import(context.Background(), recipientID, share.ID, recipientRoot.ID, idempotencyKey, accessToken)
			results <- file
			errs <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("import share: %v", err)
		}
	}
	var importedID uuid.UUID
	for file := range results {
		if importedID == uuid.Nil {
			importedID = file.ID
		} else if file.ID != importedID {
			t.Fatalf("imported file ID = %s, want %s", file.ID, importedID)
		}
	}
	var importCount, fileCount int64
	if err := db.Model(&Import{}).Where("user_id = ? AND share_id = ?", recipientID, share.ID).Count(&importCount).Error; err != nil || importCount != 1 {
		t.Fatalf("share imports = %d, err = %v", importCount, err)
	}
	if err := db.Model(&drive.FileEntry{}).Where("owner_id = ? AND folder_id = ? AND name_normalized = 'shared.txt' AND deleted_at IS NULL", recipientID, recipientRoot.ID).Count(&fileCount).Error; err != nil || fileCount != 1 {
		t.Fatalf("imported files = %d, err = %v", fileCount, err)
	}
	var recipient identity.User
	if err := db.First(&recipient, "id = ?", recipientID).Error; err != nil || recipient.UsedLogicalBytes != object.SizeBytes {
		t.Fatalf("logical usage = %d, err = %v", recipient.UsedLogicalBytes, err)
	}
	var stored objectstore.Object
	if err := db.First(&stored, "id = ?", object.ID).Error; err != nil || stored.ReferenceCount != 2 {
		t.Fatalf("object references = %d, err = %v", stored.ReferenceCount, err)
	}
}
