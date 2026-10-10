package drive

import (
	"errors"
	"testing"
	"time"

	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/google/uuid"
)

func TestCacheReadsObserveOtherReplicaMutations(t *testing.T) {
	f := newMoveFixture(t)
	reader := NewServiceWithCache(f.db, platformcache.NewJSONStore(nil, time.Minute, 8))
	if _, _, err := reader.Children(f.ownerID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.File(f.ownerID, f.file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.MoveFile(f.ownerID, f.file.ID, f.target.ID, f.file.Version); err != nil {
		t.Fatal(err)
	}
	_, files, err := reader.Children(f.ownerID, f.source.ID)
	if err != nil || len(files) != 0 {
		t.Fatalf("source cache after another replica moved file: files=%+v error=%v", files, err)
	}
	file, err := reader.File(f.ownerID, f.file.ID)
	if err != nil || file.FolderID != f.target.ID {
		t.Fatalf("file cache after another replica moved file: file=%+v error=%v", file, err)
	}
}

func TestCachedFileStillChecksAuthoritativeVisibility(t *testing.T) {
	f := newMoveFixture(t)
	reader := NewServiceWithCache(f.db, platformcache.NewJSONStore(nil, time.Minute, 8))
	if _, err := reader.File(f.ownerID, f.file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.File(uuid.New(), f.file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner read a cached file: %v", err)
	}
	if err := f.db.Exec("UPDATE file_objects SET status = 'deleting' WHERE id = ?", f.object).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := reader.File(f.ownerID, f.file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cache authorized a non-ready object: %v", err)
	}
}
