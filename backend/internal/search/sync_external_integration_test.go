package search

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestExternalLiveRebuildAndDocumentRetirement(t *testing.T) {
	if os.Getenv("TEST_SEARCH_ES_ENDPOINT") == "" || os.Getenv("TEST_SEARCH_QDRANT_ENDPOINT") == "" {
		t.Skip("external test endpoints are required")
	}
	projectionTransaction(t, func(db *gorm.DB) {
		ctx := context.Background()
		es := NewElasticsearchIndex(os.Getenv("TEST_SEARCH_ES_ENDPOINT"), "", "", nil)
		q := NewQdrantIndex(os.Getenv("TEST_SEARCH_QDRANT_ENDPOINT"), "", 1024, nil)
		external := NewExternalIndex(es, q)
		oldTarget, nextTarget := "live-old-"+uuid.NewString(), "live-next-"+uuid.NewString()
		for _, target := range []string{oldTarget, nextTarget} {
			if err := external.(EpochPreparer).PrepareEpoch(ctx, target, 1024); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = es.do(context.Background(), http.MethodDelete, "/files_"+target, nil, nil)
				_ = es.do(context.Background(), http.MethodDelete, "/chunks_"+target, nil, nil)
				_ = q.do(context.Background(), http.MethodDelete, "/collections/chunks_"+target, nil, nil)
			})
		}
		object, document, version := uuid.New(), uuid.New(), uuid.New()
		oldChunk := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
		newChunk := uuid.MustParse("00000000-0000-4000-8000-000000000001")
		vector := "[1," + strings.Repeat("0,", 1022) + "0]"
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO file_objects(id, sha256, size_bytes, mime_type, bucket, object_key, status) VALUES (?, ?, 10, 'text/plain', 'test', ?, 'ready')`, []any{object, uuid.NewString(), object.String()}},
			{`INSERT INTO ai_documents(id, object_id, status, pipeline_version, model_version) VALUES (?, ?, 'indexed', 'test', 'test')`, []any{document, object}},
			{`INSERT INTO ai_document_versions(id, document_id, object_id, pipeline_version, model_version, generation, status) VALUES (?, ?, ?, 'test', 'test', 1, 'indexed')`, []any{version, document, object}},
			{`UPDATE ai_documents SET active_version_id = ? WHERE id = ?`, []any{version, document}},
			{`INSERT INTO ai_chunks(id, document_id, document_version_id, ordinal, content, embedding) VALUES (?, ?, ?, 0, 'original accelerator', CAST(? AS vector))`, []any{oldChunk, document, version, vector}},
		} {
			if err := db.Exec(statement.sql, statement.args...).Error; err != nil {
				t.Fatal(err)
			}
		}
		p := NewProjectorWithEpoch(db, oldTarget, 1).WithExternalIndex(external)
		if _, err := p.Backfill(ctx, BackfillOptions{Activate: true}); err != nil {
			t.Fatal(err)
		}
		next := NewProjectorWithEpoch(db, nextTarget, 2).WithExternalIndex(external)
		if _, err := next.ensureBackfillVersions(ctx); err != nil {
			t.Fatal(err)
		}
		cursor, count, err := next.SnapshotContent(ctx, uuid.Nil, 1)
		if err != nil || count != 1 || cursor != oldChunk {
			t.Fatalf("snapshot cursor=%s count=%d err=%v", cursor, count, err)
		}
		newVersion := uuid.New()
		if err := db.Exec(`INSERT INTO ai_document_versions(id, document_id, object_id, pipeline_version, model_version, generation, status) VALUES (?, ?, ?, 'test', 'test', 2, 'indexed')`, newVersion, document, object).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO ai_chunks(id, document_id, document_version_id, ordinal, content, embedding) VALUES (?, ?, ?, 0, 'replacement accelerator', CAST(? AS vector))`, newChunk, document, newVersion, vector).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`UPDATE ai_documents SET active_version_id = ? WHERE id = ?`, newVersion, document).Error; err != nil {
			t.Fatal(err)
		}
		publish := func(eventType string) {
			t.Helper()
			raw, _ := json.Marshal(map[string]any{"event_id": uuid.New(), "event_type": eventType, "object_id": object})
			if err := p.Handler(ctx, taskqueue.NewTask("search.project.v1", raw)); err != nil {
				t.Fatal(err)
			}
		}
		publish(eventcontract.EventAIIndexedV1)
		// UUID keyset scans cannot revisit an insertion below their cursor.
		// Live dual writes, rather than a second scan, must bridge that gap.
		if _, count, err := next.SnapshotContent(ctx, cursor, 1); err != nil || count != 0 {
			t.Fatalf("tail count=%d err=%v", count, err)
		}
		check := func(want bool) {
			t.Helper()
			for i, target := range []string{oldTarget, nextTarget} {
				if err := es.do(ctx, http.MethodPost, "/chunks_"+target+"/_refresh", nil, nil); err != nil {
					t.Fatal(err)
				}
				for _, reader := range []RetrievalBackend{es, q} {
					hits, err := reader.SearchVector(ctx, RetrievalRequest{TargetVersion: target, IndexEpoch: int64(i + 1), Limit: 10}, append([]float32{1}, make([]float32, 1023)...))
					if err != nil {
						t.Fatal(err)
					}
					if want && (len(hits) != 1 || hits[0].ChunkID != newChunk) {
						t.Fatalf("%T/%s stale or missing chunk: %+v", reader, target, hits)
					}
					if !want && len(hits) != 0 {
						t.Fatalf("%T/%s returned GC chunk: %+v", reader, target, hits)
					}
				}
			}
		}
		check(true)
		if err := db.Exec(`UPDATE file_objects SET status = 'deleting' WHERE id = ?`, object).Error; err != nil {
			t.Fatal(err)
		}
		publish(eventcontract.EventObjectGCRequestedV1)
		check(false)
		for i := 0; i < 2; i++ {
			if err := p.RepairObject(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
		check(false)
	})
}
