package aisystem

import (
	"context"
	"fmt"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProjectionExistingVectorCollectionMustMatchGeometry(t *testing.T) {
	for _, tc := range []struct {
		name, distance string
		size           int
		success        bool
	}{{"compatible", "Cosine", 1024, true}, {"wrong-dimension", "Cosine", 768, false}, {"wrong-metric", "Dot", 1024, false}, {"missing-config", "", 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					w.WriteHeader(http.StatusConflict)
					return
				}
				_, _ = fmt.Fprintf(w, `{"status":"ok","result":{"config":{"params":{"vectors":{"size":%d,"distance":%q}}}}}`, tc.size, tc.distance)
			}))
			defer server.Close()
			err := (&Projector{QD: server.URL, HTTP: server.Client()}).ensureVectorCollection(context.Background(), "existing")
			if (err == nil) != tc.success {
				t.Fatal("existing collection geometry was not verified", err)
			}
		})
	}
}

func TestProjectionUpsertRequiresCompletedBackendResponse(t *testing.T) {
	for _, tc := range []struct {
		name, backend, body string
		success             bool
	}{
		{"es-failed-shard", "es", `{"result":"created","_shards":{"failed":1}}`, false},
		{"es-missing-proof", "es", `{}`, false},
		{"es-complete", "es", `{"result":"created","_shards":{"failed":0}}`, true},
		{"qd-pending", "qd", `{"status":"ok","result":{"status":"acknowledged"}}`, false},
		{"qd-error", "qd", `{"status":"error","result":{"status":"completed"}}`, false},
		{"qd-missing-proof", "qd", `{}`, false},
		{"qd-complete", "qd", `{"status":"ok","result":{"status":"completed"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store := databases(t)
			store.PlanContinuations = nil
			publisher, frozen, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback source"}})
			computePublish(t, publisher, &Computer{DB: db, Profile: frozen.Profile, Embedding: ai.NewProvider(config.AI{Dimension: 1024})}, claim(t, store, "ai.embedding.generate", frozen))
			var object Object
			if err := db.First(&object, "object_id=?", frozen.Intent.ObjectID).Error; err != nil {
				t.Fatal(err)
			}
			frozen.EmbeddingSetID = *object.ActiveEmbeddingSetID
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "/_doc/") || strings.HasSuffix(r.URL.Path, "/points") {
					_, _ = io.WriteString(w, tc.body)
					return
				}
				_, _ = io.WriteString(w, `{"status":"ok","result":true,"acknowledged":true}`)
			}))
			defer server.Close()
			in := frozen
			projector := &Projector{DB: db, ES: server.URL, QD: server.URL, HTTP: server.Client()}
			kind := "ai.project.text"
			if tc.backend == "qd" {
				kind = "ai.project.vector"
			}
			task := claim(t, store, kind, in)
			_, err := (&Computer{DB: db, Profile: in.Profile, Project: projector.Project}).Compute(context.Background(), task)
			if (err == nil) != tc.success {
				t.Fatalf("backend completion not verified: want success=%t err=%v", tc.success, err)
			}
			var staged int64
			if err := db.Table("ai_artifact_manifests").Where("task_id=?", task.ID).Count(&staged).Error; err != nil || (staged == 1) != tc.success {
				t.Fatal("unconfirmed projection staged as complete", staged, err)
			}
		})
	}
}

func TestProjectionExistingMappingHasNoRequestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			t.Error("GET mapping sent a body")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"test":{"mappings":{"properties":{"content_search":{"type":"text","analyzer":"whitespace"},"content_id":{"type":"keyword"},"child_id":{"type":"keyword"}}}}}`))
	}))
	defer server.Close()
	p := &Projector{ES: server.URL, HTTP: server.Client()}
	if e := p.ensureTextIndex(context.Background(), "test"); e != nil {
		t.Fatal(e)
	}
}

func TestProjectionDeleteRejectsPartialAcknowledgements(t *testing.T) {
	db, _ := databases(t)
	in := testInput()
	in.Operation = "delete"
	for _, backend := range []string{"es", "qd"} {
		if err := db.Exec(`INSERT INTO ai_orphan_projections(backend,target_id,target_version,object_id,content_generation,artifact_version_id) VALUES(?,?,?,?,?,?)`, backend, uuid.New(), "delete-fixture", in.Intent.ObjectID, 1, uuid.New()).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, backend, body string
		httpStatus          int
		success             bool
	}{
		{"es-timeout", "es", `{"timed_out":true,"version_conflicts":0,"failures":[]}`, 200, false},
		{"es-conflicts", "es", `{"timed_out":false,"version_conflicts":1,"failures":[]}`, 200, false},
		{"es-item-failure", "es", `{"timed_out":false,"version_conflicts":0,"failures":[{"cause":"fixture"}]}`, 200, false},
		{"es-malformed", "es", `{}`, 200, false},
		{"es-complete", "es", `{"timed_out":false,"version_conflicts":0,"failures":[]}`, 200, true},
		{"es-missing", "es", `{}`, 404, true},
		{"qd-acknowledged", "qd", `{"status":"ok","result":{"status":"acknowledged"}}`, 200, false},
		{"qd-error", "qd", `{"status":"error","result":{"status":"completed"}}`, 200, false},
		{"qd-complete", "qd", `{"status":"ok","result":{"status":"completed"}}`, 200, true},
		{"qd-missing", "qd", `{}`, 404, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.httpStatus)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			p := &Projector{DB: db, ES: server.URL, QD: server.URL, HTTP: server.Client()}
			_, _, err := p.projectDelete(context.Background(), tc.backend, in)
			if (err == nil) != tc.success {
				t.Fatalf("partial/malformed completion accepted: success=%t err=%v", tc.success, err)
			}
		})
	}
}
