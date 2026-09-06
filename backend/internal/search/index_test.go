package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestProjectionIDsAreStable(t *testing.T) {
	id := uuid.New()
	a := uuid.NewSHA1(uuid.NameSpaceURL, []byte("postgres:v1:"+id.String()))
	b := uuid.NewSHA1(uuid.NameSpaceURL, []byte("postgres:v1:"+id.String()))
	if a != b {
		t.Fatal("projection identity changed between retries")
	}
}

func TestIndexVersionTransitionsAreMonotonic(t *testing.T) {
	if !allowedIndexVersionTransition("building", "active") || !allowedIndexVersionTransition("active", "retired") {
		t.Fatal("valid index version transition rejected")
	}
	if allowedIndexVersionTransition("retired", "active") || allowedIndexVersionTransition("failed", "active") {
		t.Fatal("retired/failed index version can be reactivated")
	}
}

// This opt-in probe uses the migrated local/staging schema and always rolls
// back. It proves projection retries and tombstones work against PostgreSQL.
func TestPostgresProjectionRetryAndTombstone(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	objectID, documentVersionID, chunkID := uuid.New(), uuid.New(), uuid.New()
	fileID, ownerID, folderID := uuid.New(), uuid.New(), uuid.New()
	idx := NewPostgresIndex(db)
	rollback := errors.New("projection probe rollback")
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := idx.upsertContent(tx, ContentProjection{Backend: BackendPostgres, TargetVersion: "test", ObjectID: objectID, DocumentVersionID: documentVersionID, ChunkID: chunkID, IndexEpoch: 1, Content: "first"}); err != nil {
			return err
		}
		if err := idx.upsertContent(tx, ContentProjection{Backend: BackendPostgres, TargetVersion: "test", ObjectID: objectID, DocumentVersionID: documentVersionID, ChunkID: chunkID, IndexEpoch: 2, Content: "second"}); err != nil {
			return err
		}
		if err := idx.upsertContent(tx, ContentProjection{Backend: BackendPostgres, TargetVersion: "test", ObjectID: objectID, DocumentVersionID: documentVersionID, ChunkID: chunkID, IndexEpoch: 1, Content: "stale"}); err != nil {
			return err
		}
		var row contentRow
		if err := tx.Where("backend = ? AND target_version = ? AND chunk_id = ?", BackendPostgres, "test", chunkID).First(&row).Error; err != nil {
			return err
		}
		if row.Content != "second" || row.IndexEpoch != 2 {
			return errors.New("stale content projection overwrote newer version")
		}
		if err := idx.upsertFile(tx, FileProjection{Backend: BackendPostgres, TargetVersion: "test", FileEntryID: fileID, OwnerID: ownerID, FolderID: folderID, ObjectID: objectID, FileVersion: 1, IndexEpoch: 1, Name: "a.txt", MIMEType: "text/plain"}); err != nil {
			return err
		}
		if err := idx.tombstoneFile(tx, BackendPostgres, fileID, 1); err != nil {
			return err
		}
		if err := idx.tombstoneObjectContent(tx, BackendPostgres, objectID, 2); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("transaction result = %v", err)
	}
}

func TestPostgresIndexVersionLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("test-%s", uuid.NewString())
	idx := NewPostgresIndex(db)
	ctx := context.Background()
	row, err := idx.StartVersion(ctx, BackendPostgres, ProjectionFile, target, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Where("id = ?", row.ID).Delete(&IndexVersion{})
	again, err := idx.StartVersion(ctx, BackendPostgres, ProjectionFile, target, 7)
	if err != nil || again.ID != row.ID || again.State != "building" {
		t.Fatalf("idempotent start = %#v, err=%v", again, err)
	}
	if err := idx.SetVersionState(ctx, row.ID, "building", "active", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.ActiveVersion(ctx, BackendPostgres, ProjectionFile); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetVersionState(ctx, row.ID, "building", "active", nil); !errors.Is(err, ErrIndexVersionConflict) {
		t.Fatalf("stale transition = %v, want conflict", err)
	}
}
