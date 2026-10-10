package search

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
)

type contentRecorder struct {
	content       []ContentProjection
	objectDeletes int
}

func (r *contentRecorder) UpsertContent(_ context.Context, c ContentProjection) error {
	r.content = append(r.content, c)
	return nil
}
func (*contentRecorder) UpsertFile(context.Context, FileProjection) error { return nil }
func (r *contentRecorder) TombstoneObjectContent(context.Context, string, uuid.UUID, int64) error {
	r.objectDeletes++
	return nil
}
func (*contentRecorder) TombstoneFile(context.Context, string, uuid.UUID, int64) error { return nil }
func (*contentRecorder) Health(context.Context) error                                  { return nil }

func TestGCProjectionPreservesRevivedObjectGeneration(t *testing.T) {
	db, err := gorm.Open(postgres.Open(testPostgresDSN(t)), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	target := "gc-" + uuid.NewString()
	if err := db.Exec("INSERT INTO file_objects(id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count, version) VALUES (?, ?, 10, 'text/plain', 'test', ?, 'ready', 0, 1)", id, uuid.NewString(), id.String()).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM search_content_projections WHERE target_version = ?", target).Error
		_ = db.Exec("DELETE FROM file_objects WHERE id = ?", id).Error
	})
	p := NewProjector(db, target)
	old := ContentProjection{Backend: BackendPostgres, TargetVersion: target, ObjectID: id, ChunkID: uuid.New(), DocumentVersionID: uuid.New(), IndexEpoch: 1, Content: "old"}
	if err := p.index.UpsertContent(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(eventcontract.ObjectGCRequestedV1{ObjectID: id})
	project := func() {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			return p.project(tx, uuid.New(), eventcontract.EventObjectGCRequestedV1, nil, payload)
		}); err != nil {
			t.Fatal(err)
		}
	}
	project()
	var row contentRow
	if err := db.Where("chunk_id = ?", old.ChunkID).First(&row).Error; err != nil || row.State != "indexed" {
		t.Fatalf("GC request retired a still-ready object: %+v %v", row, err)
	}
	if err := db.Exec("UPDATE file_objects SET status='deleting' WHERE id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	project()
	if err := db.Where("chunk_id = ?", old.ChunkID).First(&row).Error; err != nil || row.State != "tombstoned" || row.IndexEpoch != 1 {
		t.Fatalf("deletion watermark=%+v error=%v", row, err)
	}
	if err := db.Exec("UPDATE file_objects SET status='ready', version=4, object_key=? WHERE id = ?", "new-"+id.String(), id).Error; err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.ChunkID = uuid.New()
	newer.DocumentVersionID = uuid.New()
	newer.Content = "new"
	if err := p.index.UpsertContent(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	project()
	recorder := &contentRecorder{}
	p.WithExternalIndex(recorder)
	if err := p.publishExternal(context.Background(), eventcontract.EventObjectGCRequestedV1, nil, payload); err != nil {
		t.Fatal(err)
	}
	if recorder.objectDeletes != 0 || len(recorder.content) != 2 {
		t.Fatalf("object-wide deletion across generations: %+v", recorder)
	}
	for _, c := range recorder.content {
		if c.Tombstone != (c.ChunkID == old.ChunkID) {
			t.Fatalf("wrong generation tombstone: %+v", c)
		}
	}
}

func TestProjectorParsesConfiguredEpoch(t *testing.T) {
	if got := NewProjector(nil, "v9").indexEpoch; got != 9 {
		t.Fatalf("epoch=%d", got)
	}
}
