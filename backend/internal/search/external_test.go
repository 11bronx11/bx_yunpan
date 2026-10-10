package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestElasticsearchUpsertUsesStableVersionedDocument(t *testing.T) {
	var gotPath string
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	fileID, ownerID, folderID, objectID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	index := NewElasticsearchIndex(server.URL, "user", "secret", server.Client())
	err := index.UpsertFile(context.Background(), FileProjection{TargetVersion: "v2", FileEntryID: fileID, OwnerID: ownerID, FolderID: folderID, ObjectID: objectID, FileVersion: 4, ACLVersion: 3, Name: "note.txt", MIMEType: "text/plain", IndexEpoch: 2})
	if err != nil {
		t.Fatal(err)
	}
	doc := got["script"].(map[string]any)["params"].(map[string]any)["doc"].(map[string]any)
	if gotPath != "/files_v2/_update/"+fileID.String() || doc["file_entry_id"] != fileID.String() || doc["acl_version"].(float64) != 3 {
		t.Fatalf("unexpected request path/body: %s %#v", gotPath, got)
	}
}

func TestExternalHTTPErrorsClassifyRetryableResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer server.Close()
	index := NewElasticsearchIndex(server.URL, "", "", server.Client())
	err := index.Health(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want unavailable", err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || !httpErr.Retryable {
		t.Fatalf("error = %#v, want retryable HTTPError", err)
	}

	timedOut := NewElasticsearchIndex(server.URL, "", "", &http.Client{Timeout: time.Nanosecond})
	if err := timedOut.Health(context.Background()); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestQdrantRejectsWrongDimensionBeforeNetwork(t *testing.T) {
	index := NewQdrantIndex("http://127.0.0.1:1", "", 3, nil)
	_, err := index.SearchVector(context.Background(), RetrievalRequest{TargetVersion: "v1", Limit: 2}, []float32{1, 2})
	if err == nil {
		t.Fatal("expected dimension error")
	}
}

func TestElasticsearchVectorSearchUsesBoundedKNN(t *testing.T) {
	var body map[string]any
	chunkID, objectID, versionID := uuid.New(), uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chunks_v7/_mget" {
			_, _ = w.Write([]byte(`{"docs":[{"found":false}]}`))
			return
		}
		if r.URL.Path != "/chunks_v7/_search" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"hits":{"hits":[{"_score":0.8,"_source":{"chunk_id":"` + chunkID.String() + `","object_id":"` + objectID.String() + `","document_version_id":"` + versionID.String() + `"}}]}}`))
	}))
	defer server.Close()
	index := NewElasticsearchIndex(server.URL, "", "", server.Client())
	result, err := index.SearchVector(context.Background(), RetrievalRequest{TargetVersion: "v7", Limit: 20, Oversample: 4, Timeout: time.Second}, []float32{0.1, 0.2})
	if err != nil || len(result) != 1 || result[0].ChunkID != chunkID {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	knn, ok := body["knn"].(map[string]any)
	if !ok || knn["field"] != "embedding" || int(knn["k"].(float64)) != 20 || int(knn["num_candidates"].(float64)) != 80 {
		t.Fatalf("unexpected knn body: %#v", body)
	}
}

func TestElasticsearchBulkReportsOnlyFailedItems(t *testing.T) {
	chunkA, chunkB := uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_bulk" || r.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Fatalf("unexpected bulk request: %s %s", r.URL.Path, r.Header.Get("Content-Type"))
		}
		_, _ = w.Write([]byte(`{"items":[{"update":{"_id":"` + chunkA.String() + `","status":200,"result":"updated"}},{"update":{"_id":"` + chunkB.String() + `","status":429,"error":{"type":"too_many_requests"}}}]}`))
	}))
	defer server.Close()
	index := NewElasticsearchIndex(server.URL, "", "", server.Client())
	failed := index.BulkUpsertContent(context.Background(), []ContentProjection{{ChunkID: chunkA, ObjectID: uuid.New(), DocumentVersionID: uuid.New(), TargetVersion: "v1", IndexEpoch: 1}, {ChunkID: chunkB, ObjectID: uuid.New(), DocumentVersionID: uuid.New(), TargetVersion: "v1", IndexEpoch: 1}})
	if len(failed) != 1 || failed[0].ID != chunkB.String() || !failed[0].Retryable {
		t.Fatalf("failed = %#v", failed)
	}
}

func TestElasticsearchPrepareEpochCreatesVersionedMappings(t *testing.T) {
	paths := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path == "/chunks_v9" {
			properties := body["mappings"].(map[string]any)["properties"].(map[string]any)
			if properties["embedding"].(map[string]any)["dims"].(float64) != 1024 {
				t.Fatalf("unexpected embedding mapping: %#v", properties["embedding"])
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := NewElasticsearchIndex(server.URL, "", "", server.Client()).PrepareEpoch(context.Background(), "v9", 1024); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/files_v9" || paths[1] != "/chunks_v9" {
		t.Fatalf("mapping paths = %#v", paths)
	}
}

func TestElasticsearchPrepareEpochIgnoresOnlyExistingIndex400(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"resource_already_exists_exception","reason":"index already exists"},"status":400}`))
	}))
	defer server.Close()
	if err := NewElasticsearchIndex(server.URL, "", "", server.Client()).PrepareEpoch(context.Background(), "resume-v9", 1024); err != nil {
		t.Fatalf("existing index response should be idempotent: %v", err)
	}
	if calls != 2 {
		t.Fatalf("mapping calls = %d, want 2", calls)
	}
}

func TestElasticsearchPrepareEpochDoesNotHideMapping400(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"mapper_parsing_exception","reason":"bad mapping"},"status":400}`))
	}))
	defer server.Close()
	err := NewElasticsearchIndex(server.URL, "", "", server.Client()).PrepareEpoch(context.Background(), "bad-v9", 1024)
	if err == nil {
		t.Fatal("expected mapping error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.ErrorType != "mapper_parsing_exception" || httpErr.Reason != "bad mapping" {
		t.Fatalf("error = %#v, want mapping classification", err)
	}
}

func TestQdrantBulkRejectsInvalidAndKeepsStableIDs(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/collections/chunks_v1" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/collections/chunks_v1/points" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/collections/chunks_v1/points/scroll" {
			_, _ = w.Write([]byte(`{"result":{"points":[],"next_page_offset":null}}`))
			return
		}
		t.Fatalf("unexpected path %s", r.URL.Path)
	}))
	defer server.Close()
	index := NewQdrantIndex(server.URL, "", 2, server.Client())
	bad := uuid.New()
	good := uuid.New()
	badVector := "[1]"
	goodVector := "[1,0]"
	failed := index.BulkUpsertContent(context.Background(), []ContentProjection{{ChunkID: bad, ObjectID: uuid.New(), TargetVersion: "v1", IndexEpoch: 1, VectorRef: &badVector}, {ChunkID: good, ObjectID: uuid.New(), TargetVersion: "v1", IndexEpoch: 1, VectorRef: &goodVector}})
	if len(failed) != 1 || failed[0].ID != bad.String() || calls != 3 {
		t.Fatalf("failed = %#v calls=%d", failed, calls)
	}
}
