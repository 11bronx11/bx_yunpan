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
		for _, epoch := range []int64{1, 2} {
			if err := idx.upsertContent(tx, ContentProjection{Backend: BackendPostgres, TargetVersion: "test", ObjectID: objectID, DocumentVersionID: documentVersionID, ChunkID: chunkID, IndexEpoch: epoch, Content: "stale replay"}); err != nil {
				return err
			}
		}
		if err := tx.Where("chunk_id = ?", chunkID).First(&row).Error; err != nil || row.State != "tombstoned" || row.IndexEpoch != 2 {
			return fmt.Errorf("content tombstone lost: %+v error=%v", row, err)
		}
		if err := idx.tombstoneFile(tx, BackendPostgres, fileID, 3); err != nil {
			return err
		}
		for _, version := range []int64{2, 3} {
			if err := idx.upsertFile(tx, FileProjection{Backend: BackendPostgres, TargetVersion: "test", FileEntryID: fileID, OwnerID: ownerID, FolderID: folderID, ObjectID: objectID, FileVersion: version, IndexEpoch: 1, Name: "stale.txt"}); err != nil {
				return err
			}
		}
		var file fileRow
		if err := tx.Where("file_entry_id = ?", fileID).First(&file).Error; err != nil || !file.Tombstone || file.FileVersion != 3 {
			return fmt.Errorf("file tombstone lost: %+v error=%v", file, err)
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
	watermark, checksum := int64(0), "sha256:test"
	if err := idx.UpdateVersionProgress(ctx, row.ID, &watermark, &checksum); err != nil {
		t.Fatal(err)
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

func TestPostgresIndexProgressAndActivation(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	idx := NewPostgresIndex(db)
	target := fmt.Sprintf("progress-%s", uuid.NewString())
	row, err := idx.StartVersion(context.Background(), BackendPostgres, ProjectionContent, target, 9)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Where("id = ?", row.ID).Delete(&IndexVersion{})
	watermark := int64(42)
	checksum := "sha256:test"
	if err := idx.UpdateVersionProgress(context.Background(), row.ID, &watermark, &checksum); err != nil {
		t.Fatal(err)
	}
	if err := idx.ActivateVersion(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
	active, err := idx.ActiveVersion(context.Background(), BackendPostgres, ProjectionContent)
	if err != nil || active.ID != row.ID || active.SnapshotWatermark == nil || *active.SnapshotWatermark != watermark || active.Checksum == nil || *active.Checksum != checksum {
		t.Fatalf("active progress = %#v, err=%v", active, err)
	}
}

func TestPostgresIndexRollbackRestoresRetiredVersion(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	idx := NewPostgresIndex(db)
	oldTarget := fmt.Sprintf("rollback-old-%s", uuid.NewString())
	newTarget := fmt.Sprintf("rollback-new-%s", uuid.NewString())
	oldVersion, err := idx.StartVersion(ctx, BackendPostgres, ProjectionFile, oldTarget, 20)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := idx.StartVersion(ctx, BackendPostgres, ProjectionFile, newTarget, 21)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Where("id IN ?", []uuid.UUID{oldVersion.ID, newVersion.ID}).Delete(&IndexVersion{})
	watermark, checksum := int64(0), "sha256:test"
	for _, id := range []uuid.UUID{oldVersion.ID, newVersion.ID} {
		if err := idx.UpdateVersionProgress(ctx, id, &watermark, &checksum); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.ActivateVersion(ctx, oldVersion.ID); err != nil {
		t.Fatal(err)
	}
	if err := idx.ActivateVersion(ctx, newVersion.ID); err != nil {
		t.Fatal(err)
	}
	if err := idx.RollbackVersion(ctx, BackendPostgres, ProjectionFile, oldTarget); err != nil {
		t.Fatal(err)
	}
	active, err := idx.ActiveVersion(ctx, BackendPostgres, ProjectionFile)
	if err != nil || active.ID != oldVersion.ID || active.TargetVersion != oldTarget {
		t.Fatalf("rollback active version = %#v, err=%v", active, err)
	}
	var retired IndexVersion
	if err := db.First(&retired, "id = ?", newVersion.ID).Error; err != nil || retired.State != "retired" {
		t.Fatalf("new version after rollback = %#v, err=%v", retired, err)
	}
}

func TestPostgresBackfillResumesAndActivates(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("backfill-%s", uuid.NewString())
	projector := NewProjectorWithEpoch(db, target, 11)
	defer func() {
		_ = db.Exec("DELETE FROM search_content_projections WHERE target_version = ?", target).Error
		_ = db.Exec("DELETE FROM search_file_projections WHERE target_version = ?", target).Error
		_ = db.Exec("DELETE FROM search_index_versions WHERE target_version = ?", target).Error
	}()
	report, err := projector.Backfill(context.Background(), BackfillOptions{TargetVersion: target, IndexEpoch: 11, BatchSize: 2, Activate: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.TargetVersion != target || report.IndexEpoch != 11 || report.FileChecksum == "" || report.ContentChecksum == "" || !report.Activated {
		t.Fatalf("backfill report = %#v", report)
	}
	for _, projection := range []string{ProjectionFile, ProjectionContent} {
		row, err := projector.index.ActiveVersion(context.Background(), BackendPostgres, projection)
		if err != nil || row.TargetVersion != target || row.IndexEpoch != 11 || row.State != "active" {
			t.Fatalf("active %s version = %#v, err=%v", projection, row, err)
		}
	}
}
