package drive

import (
	"context"
	"errors"
	"testing"
	"time"

	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/google/uuid"
)

func TestCachedReadsUseCurrentAIStatus(t *testing.T) {
	f := newMoveFixture(t)
	ctx := context.Background()
	store := platformcache.NewJSONStore(nil, time.Hour, 8)
	reader := NewServiceWithCache(f.db, store)
	for _, want := range []string{"", "processing", "indexed", "processing", "failed", ""} {
		if want == "" {
			if err := f.db.Exec("DELETE FROM ai_documents WHERE object_id = ?", f.object).Error; err != nil {
				t.Fatal(err)
			}
		} else if err := f.db.Exec(`INSERT INTO ai_documents (id, object_id, status, pipeline_version, model_version)
			VALUES (?, ?, ?, 'test', 'test') ON CONFLICT (object_id) DO UPDATE SET status = EXCLUDED.status`,
			uuid.New(), f.object, want).Error; err != nil {
			t.Fatal(err)
		}
		_, children, err := reader.Children(f.ownerID, f.source.ID)
		if err != nil || len(children) != 1 {
			t.Fatalf("children=%v err=%v", children, err)
		}
		_, page, _, _, err := reader.ChildrenPage(ctx, f.ownerID, f.source.ID, "", 50, "")
		if err != nil || len(page) != 1 {
			t.Fatalf("page=%v err=%v", page, err)
		}
		file, err := reader.File(f.ownerID, f.file.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []FileView{children[0], page[0], file} {
			if (want == "" && got.AIStatus != nil) || (want != "" && (got.AIStatus == nil || *got.AIStatus != want)) {
				t.Fatalf("AIStatus=%v, want %q while metadata cache is still valid", got.AIStatus, want)
			}
			if want != "" && FileResponse(got)["ai_status"] != want {
				t.Fatalf("response lost ai_status: %v", FileResponse(got))
			}
		}
	}
	// A fresh cache fill must also exclude the volatile field itself.
	if err := store.Delete(ctx, reader.readCacheKey(f.ownerID, f.file.ID, "file")); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`INSERT INTO ai_documents (id, object_id, status, pipeline_version, model_version)
		VALUES (?, ?, 'indexed', 'test', 'test')`, uuid.New(), f.object).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := reader.File(f.ownerID, f.file.ID); err != nil {
		t.Fatal(err)
	}
	var cached map[string]any
	if hit, err := store.Get(ctx, reader.readCacheKey(f.ownerID, f.file.ID, "file"), &cached); err != nil || !hit {
		t.Fatalf("metadata cache hit=%v err=%v", hit, err)
	}
	if _, exists := cached["AIStatus"]; exists {
		t.Fatal("AIStatus was stored in the metadata cache")
	}
}

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
