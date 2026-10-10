package search

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestExternalContainers is an opt-in contract gate for the Compose
// search-external profile. Unit tests stay offline unless both endpoints are
// explicitly injected by the caller.
func TestExternalContainers(t *testing.T) {
	esEndpoint := strings.TrimRight(os.Getenv("TEST_SEARCH_ES_ENDPOINT"), "/")
	qdrantEndpoint := strings.TrimRight(os.Getenv("TEST_SEARCH_QDRANT_ENDPOINT"), "/")
	if esEndpoint == "" || qdrantEndpoint == "" {
		t.Skip("TEST_SEARCH_ES_ENDPOINT and TEST_SEARCH_QDRANT_ENDPOINT are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	es := NewElasticsearchIndex(esEndpoint, "", "", client)
	qdrant := NewQdrantIndex(qdrantEndpoint, "", 2, client)
	target := "probe-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")

	if err := es.Health(ctx); err != nil {
		t.Fatalf("elasticsearch health: %v", err)
	}
	if err := qdrant.Health(ctx); err != nil {
		t.Fatalf("qdrant health: %v", err)
	}
	if err := es.PrepareEpoch(ctx, target, 2); err != nil {
		t.Fatalf("elasticsearch epoch: %v", err)
	}
	if err := qdrant.PrepareEpoch(ctx, target, 2); err != nil {
		t.Fatalf("qdrant collection: %v", err)
	}
	t.Cleanup(func() {
		for _, endpoint := range []string{
			esEndpoint + "/files_" + target,
			esEndpoint + "/chunks_" + target,
			qdrantEndpoint + "/collections/chunks_" + target,
		} {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, endpoint, nil)
			if err != nil {
				continue
			}
			response, err := client.Do(req)
			if err == nil {
				response.Body.Close()
			}
		}
	})

	fileID, ownerID, folderID, objectID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	chunkID, versionID := uuid.New(), uuid.New()
	vector := "[1,0]"
	file := FileProjection{TargetVersion: target, FileEntryID: fileID, OwnerID: ownerID, FolderID: folderID, ObjectID: objectID, FileVersion: 1, ACLVersion: 1, Name: "external probe", MIMEType: "text/plain", IndexEpoch: 1}
	content := ContentProjection{TargetVersion: target, ObjectID: objectID, DocumentVersionID: versionID, ChunkID: chunkID, Ordinal: 0, PipelineVersion: "probe", ModelVersion: "probe", IndexEpoch: 1, Content: "external adapter probe text", VectorRef: &vector}
	if err := es.UpsertFile(ctx, file); err != nil {
		t.Fatalf("elasticsearch file upsert: %v", err)
	}
	if err := es.UpsertContent(ctx, content); err != nil {
		t.Fatalf("elasticsearch content upsert: %v", err)
	}
	if err := es.UpsertContent(ctx, content); err != nil {
		t.Fatalf("elasticsearch duplicate content upsert: %v", err)
	}
	refresh, err := http.NewRequestWithContext(ctx, http.MethodPost, esEndpoint+"/chunks_"+target+"/_refresh", nil)
	if err != nil {
		t.Fatalf("elasticsearch refresh request: %v", err)
	}
	response, err := client.Do(refresh)
	if err != nil {
		t.Fatalf("elasticsearch refresh: %v", err)
	}
	response.Body.Close()
	textHits, err := es.SearchText(ctx, RetrievalRequest{TargetVersion: target, Query: "external adapter", Limit: 5})
	if err != nil || len(textHits) != 1 || textHits[0].ChunkID != chunkID {
		t.Fatalf("elasticsearch text hits=%#v err=%v", textHits, err)
	}

	if err := qdrant.UpsertContent(ctx, content); err != nil {
		t.Fatalf("qdrant content upsert: %v", err)
	}
	if err := qdrant.UpsertContent(ctx, content); err != nil {
		t.Fatalf("qdrant duplicate content upsert: %v", err)
	}
	vectorHits, err := qdrant.SearchVector(ctx, RetrievalRequest{TargetVersion: target, Limit: 5}, []float32{1, 0})
	if err != nil || len(vectorHits) != 1 || vectorHits[0].ChunkID != chunkID {
		t.Fatalf("qdrant vector hits=%#v err=%v", vectorHits, err)
	}
}
