package drive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/google/uuid"
)

func TestCachedPagesObserveBackgroundContentEpoch(t *testing.T) {
	f := seedPageFixture(t)
	ctx := context.Background()
	reader := NewServiceWithCache(f.db, platformcache.NewJSONStore(nil, time.Hour, 16))
	if _, files, err := reader.Children(f.ownerID, f.source.ID); err != nil || len(files) != 1 {
		t.Fatalf("prime children: files=%v err=%v", files, err)
	}
	if _, files, _, _, err := reader.ChildrenPage(ctx, f.ownerID, f.source.ID, "", 50, ""); err != nil || len(files) != 1 {
		t.Fatalf("prime page: files=%v err=%v", files, err)
	}
	_, _, more, cursor, err := reader.ChildrenPage(ctx, f.ownerID, f.target.ID, "", 3, "")
	if err != nil || !more || cursor == "" {
		t.Fatalf("prime cursor: more=%v cursor=%q err=%v", more, cursor, err)
	}
	// The committer writes the independent background counter. An API cache
	// primed before that commit must observe the new entry immediately.
	background := f.db.WithContext(ownerepoch.Background(ctx))
	created, err := f.service.CreateFile(background, f.ownerID, f.source.ID, createMoveObject(t, background), "background.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerepoch.BumpContent(background, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, files, err := reader.Children(f.ownerID, f.source.ID); err != nil || len(files) != 2 {
		t.Fatalf("background entry %s missing from cached children: files=%v err=%v", created.ID, files, err)
	}
	if _, files, _, _, err := reader.ChildrenPage(ctx, f.ownerID, f.source.ID, "", 50, ""); err != nil || len(files) != 2 {
		t.Fatalf("background entry missing from cached page: files=%v err=%v", files, err)
	}
	if _, _, _, _, err := reader.ChildrenPage(ctx, f.ownerID, f.target.ID, cursor, 3, ""); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("background commit left old cursor valid: %v", err)
	}
}

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
