package search

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestExternalTombstonesSurviveReorderedWrites(t *testing.T) {
	if os.Getenv("TEST_SEARCH_ES_ENDPOINT") == "" || os.Getenv("TEST_SEARCH_QDRANT_ENDPOINT") == "" {
		t.Skip("external test endpoints are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	es := NewElasticsearchIndex(os.Getenv("TEST_SEARCH_ES_ENDPOINT"), "", "", nil)
	q := NewQdrantIndex(os.Getenv("TEST_SEARCH_QDRANT_ENDPOINT"), "", 2, nil)
	target := "fencing-" + uuid.NewString()
	for _, index := range []EpochPreparer{es, q} {
		if err := index.PrepareEpoch(ctx, target, 2); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = es.do(context.Background(), http.MethodDelete, "/files_"+target, nil, nil)
		_ = es.do(context.Background(), http.MethodDelete, "/chunks_"+target, nil, nil)
		_ = q.do(context.Background(), http.MethodDelete, "/collections/chunks_"+target, nil, nil)
	})
	file := FileProjection{TargetVersion: target, FileEntryID: uuid.New(), OwnerID: uuid.New(), ObjectID: uuid.New(), FolderID: uuid.New(), FileVersion: 1, ACLVersion: 1, IndexEpoch: 1, Name: "old"}
	if err := es.TombstoneFile(ctx, target, file.FileEntryID, 3); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{1, 2, 3} {
		file.FileVersion = version
		if err := es.UpsertFile(ctx, file); err != nil {
			t.Fatal(err)
		}
		if failed := es.BulkUpsertFile(ctx, []FileProjection{file}); len(failed) != 0 {
			t.Fatal(failed)
		}
	}
	var doc struct {
		Source struct {
			Version   int64  `json:"file_version"`
			Tombstone bool   `json:"tombstone"`
			Name      string `json:"name"`
		} `json:"_source"`
	}
	if err := es.do(ctx, http.MethodGet, "/files_"+target+"/_doc/"+file.FileEntryID.String(), nil, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Source.Tombstone || doc.Source.Version != 3 {
		t.Fatalf("file resurrected: %+v", doc)
	}
	file.FileVersion = 4
	file.Name = "new"
	if err := es.UpsertFile(ctx, file); err != nil {
		t.Fatal(err)
	}
	if err := es.TombstoneFile(ctx, target, file.FileEntryID, 2); err != nil {
		t.Fatal(err)
	}
	if err := es.do(ctx, http.MethodGet, "/files_"+target+"/_doc/"+file.FileEntryID.String(), nil, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Source.Tombstone || doc.Source.Version != 4 || doc.Source.Name != "new" {
		t.Fatalf("late delete won: %+v", doc)
	}

	vector := "[1,0]"
	for _, beforeInsert := range []bool{false, true} {
		for _, backend := range []struct {
			name  string
			index Index
			read  RetrievalBackend
		}{{"es", es, es}, {"qdrant", q, q}} {
			t.Run(fmt.Sprintf("%s/delete_before_insert=%v", backend.name, beforeInsert), func(t *testing.T) {
				content := ContentProjection{TargetVersion: target, ObjectID: uuid.New(), DocumentVersionID: uuid.New(), ChunkID: uuid.New(), IndexEpoch: 1, Content: "fencing test", VectorRef: &vector}
				if !beforeInsert {
					if err := backend.index.UpsertContent(ctx, content); err != nil {
						t.Fatal(err)
					}
				}
				if err := backend.index.TombstoneObjectContent(ctx, target, content.ObjectID, 2); err != nil {
					t.Fatal(err)
				}
				// New adapter instances model independent workers; there is no local lock.
				var writer Index = NewQdrantIndex(q.endpoint, "", 2, nil)
				if backend.name == "es" {
					writer = NewElasticsearchIndex(es.endpoint, "", "", nil)
				}
				for _, epoch := range []int64{1, 2} {
					content.IndexEpoch = epoch
					if err := writer.UpsertContent(ctx, content); err != nil {
						t.Fatal(err)
					}
					if backend.name == "es" {
						if failed := es.BulkUpsertContent(ctx, []ContentProjection{content}); len(failed) != 0 {
							t.Fatal(failed)
						}
					} else {
						if failed := q.BulkUpsertContent(ctx, []ContentProjection{content}); len(failed) != 0 {
							t.Fatal(failed)
						}
					}
					if backend.name == "es" {
						if err := es.do(ctx, http.MethodPost, "/chunks_"+target+"/_refresh", nil, nil); err != nil {
							t.Fatal(err)
						}
					}
					hits, err := backend.read.SearchVector(ctx, RetrievalRequest{TargetVersion: target, IndexEpoch: epoch, Limit: 100}, []float32{1, 0})
					if err != nil {
						t.Fatal(err)
					}
					for _, hit := range hits {
						if hit.ChunkID == content.ChunkID {
							t.Fatalf("deleted chunk returned: %+v", hit)
						}
					}
				}
				// An older deletion must not overwrite the newer deletion watermark.
				if err := writer.TombstoneObjectContent(ctx, target, content.ObjectID, 1); err != nil {
					t.Fatal(err)
				}
				content.IndexEpoch = 3
				if err := writer.UpsertContent(ctx, content); err != nil {
					t.Fatal(err)
				}
				if backend.name == "es" {
					if err := es.do(ctx, http.MethodPost, "/chunks_"+target+"/_refresh", nil, nil); err != nil {
						t.Fatal(err)
					}
				}
				hits, err := backend.read.SearchVector(ctx, RetrievalRequest{TargetVersion: target, IndexEpoch: 3, Limit: 100}, []float32{1, 0})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, hit := range hits {
					found = found || hit.ChunkID == content.ChunkID
				}
				if !found {
					t.Fatal("new generation suppressed by old tombstone")
				}
				content.Tombstone = true
				if err := writer.UpsertContent(ctx, content); err != nil {
					t.Fatal(err)
				}
				content.Tombstone = false
				if err := writer.UpsertContent(ctx, content); err != nil {
					t.Fatal(err)
				}
				if backend.name == "es" {
					if err := es.do(ctx, http.MethodPost, "/chunks_"+target+"/_refresh", nil, nil); err != nil {
						t.Fatal(err)
					}
				}
				hits, err = backend.read.SearchVector(ctx, RetrievalRequest{TargetVersion: target, IndexEpoch: 3, Limit: 100}, []float32{1, 0})
				if err != nil {
					t.Fatal(err)
				}
				for _, hit := range hits {
					if hit.ChunkID == content.ChunkID {
						t.Fatal("same-generation chunk tombstone overwritten")
					}
				}
			})
		}
	}
	// Different revisions in flight must always leave the largest file version.
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for version := int64(5); version < 17; version++ {
		copy := file
		copy.FileVersion = version
		copy.Name = fmt.Sprint(version)
		wg.Add(1)
		go func() { defer wg.Done(); errs <- es.UpsertFile(ctx, copy) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := es.do(ctx, http.MethodGet, "/files_"+target+"/_doc/"+file.FileEntryID.String(), nil, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Source.Version != 16 {
		t.Fatalf("concurrent version lost: %+v", doc)
	}
}
