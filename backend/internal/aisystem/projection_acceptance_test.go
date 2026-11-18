package aisystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func projectionBackend(t *testing.T, db *gorm.DB) *Projector {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/points") {
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","result":true,"acknowledged":true}`))
	}))
	t.Cleanup(server.Close)
	return &Projector{DB: db, ES: server.URL, QD: server.URL, HTTP: server.Client()}
}

func acceptProjection(t *testing.T, p *Projector, kind string, in aiprotocol.Input) {
	t.Helper()
	data, count, err := p.Project(context.Background(), kind, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.DB.Transaction(func(tx *gorm.DB) error {
		return publishProjection(tx, in, Artifact{Kind: kind, Data: jsonValue(data), RowCount: count})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionRejectsMislabeledEmbeddingSet(t *testing.T) {
	db, store := databases(t)
	store.PlanContinuations = nil
	publisher, in, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback"}})
	in = publishRecallVectors(t, db, store, publisher, in, 1)
	p := projectionBackend(t, db)
	for _, field := range []string{"provider", "model", "revision", "dimension"} {
		t.Run(field, func(t *testing.T) {
			wrong := in
			switch field {
			case "provider":
				wrong.Profile.EmbeddingProvider = "other"
			case "model":
				wrong.Profile.EmbeddingModel = "other"
			case "revision":
				wrong.Profile.EmbeddingRevision = "other"
			case "dimension":
				wrong.Profile.Dimension = 768
			}
			if _, _, err := p.Project(context.Background(), "ai.project.vector", wrong); err == nil {
				t.Fatal("embedding set projected under a different model identity")
			}
		})
	}
}

func TestProjectionMixedEmbeddingProfilesDoNotBlockCutover(t *testing.T) {
	db, store := databases(t)
	store.PlanContinuations = nil
	publisher, first, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "old model"}})
	first = publishRecallVectors(t, db, store, publisher, first, 1)
	p := projectionBackend(t, db)
	acceptProjection(t, p, "ai.project.vector", first)
	_, second, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "new model"}})
	second.Profile.EmbeddingRevision = "v2"
	second.TargetEpoch = 2
	second = publishRecallVectors(t, db, store, publisher, second, 1)
	acceptProjection(t, p, "ai.project.vector", second)
	_, expected, _, _ := projectionIdentity("qd", second)
	var active string
	if err := db.Table("ai_published_targets").Select("target_version").Where("backend='qd'").Scan(&active).Error; err != nil || active != expected {
		t.Fatalf("old model stranded fully covered new target: active=%q expected=%q err=%v", active, expected, err)
	}
	// A late completion from an earlier epoch cannot roll the active target back.
	acceptProjection(t, p, "ai.project.vector", first)
	if err := db.Table("ai_published_targets").Select("target_version").Where("backend='qd'").Scan(&active).Error; err != nil || active != expected {
		t.Fatal("late old epoch replaced new vector target", active, err)
	}
}

func TestProjectionQueryChecksVectorTargetIdentityAndStatus(t *testing.T) {
	db, store := databases(t)
	store.PlanContinuations = nil
	publisher, in, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback"}})
	in = publishRecallVectors(t, db, store, publisher, in, 1)
	p := projectionBackend(t, db)
	acceptProjection(t, p, "ai.project.vector", in)
	calls := 0
	response := `{"status":"ok","result":{"points":[]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()
	p.QD, p.HTTP = server.URL, server.Client()
	versions := []string{in.EmbeddingSetID.String()}
	vector := make([]float32, 1024)
	vector[0] = 1
	if _, _, err := p.external(context.Background(), "qd", versions, "rollback", vector, 10); err != nil {
		t.Fatal("valid empty query rejected", err)
	}
	for _, body := range []string{`{"status":"error","result":{"points":[]}}`, `{"result":{"points":[]}}`, `{"status":"ok","result":{}}`} {
		response = body
		if _, _, err := p.external(context.Background(), "qd", versions, "rollback", vector, 10); err == nil {
			t.Error("invalid Qdrant query accepted as an empty result", body)
		}
	}
	response = `{"status":"ok","result":{"points":[]}}`
	if err := db.Exec("UPDATE ai_projection_targets SET config_digest=? WHERE backend='qd'", aiprotocol.HashJSON("different-model")).Error; err != nil {
		t.Fatal(err)
	}
	calls = 0
	if _, _, err := p.external(context.Background(), "qd", versions, "rollback", vector, 10); err == nil || calls != 0 {
		t.Fatal("incompatible vector target reached Qdrant", calls, err)
	}
}

func TestProjectionLegacyCompletionCannotReplaceCurrentTextTarget(t *testing.T) {
	db, _, _, in, _ := readyRetrievalBlocks(t, []rag.Section{{Text: "rollback", HeadingPath: "current heading"}})
	var rows []indexedChild
	if err := db.Table("ai_child_chunks").Select("id,parent_id,content,ordinal,heading_path").Where("content_version_id=?", in.ContentVersionID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	currentID, currentName, digest, _ := projectionIdentity("es", in)
	for _, schema := range []string{textProjectionSchema, "text-schema-v1"} {
		id, name, config, _ := projectionIdentityForSchema("es", in, schema)
		data := projectionData{Backend: "es", TargetID: id, TargetVersion: name, ConfigDigest: config, Epoch: in.TargetEpoch, ContentID: in.ContentVersionID, Count: len(rows), Hash: projectionRowsHash(rows, config == digest), Operation: "upsert"}
		raw, _ := json.Marshal(data)
		if err := db.Transaction(func(tx *gorm.DB) error {
			return publishProjection(tx, in, Artifact{Kind: "ai.project.text", Data: raw, RowCount: int64(len(rows))})
		}); err != nil {
			t.Fatal("retained accepted result could not publish", schema, err)
		}
	}
	var active string
	if err := db.Table("ai_published_targets").Select("target_version").Where("id=?", currentID).Scan(&active).Error; err != nil || active != currentName {
		t.Fatal("late legacy result disabled the current text projection", active, err)
	}
}

// Real backends, isolated PG schemas and unique physical targets. A failed
// second write leaves physical bytes but must not create a completion proof.
func TestProjectionRealBackendsRetryPublicationAndDelete(t *testing.T) {
	for _, backend := range []string{"es", "qd"} {
		t.Run(backend, func(t *testing.T) {
			key, kind, children := "TEST_SEARCH_ES_ENDPOINT", "ai.project.text", 3
			if backend == "qd" {
				key, kind, children = "TEST_SEARCH_QDRANT_ENDPOINT", "ai.project.vector", 70
			}
			endpoint := os.Getenv(key)
			if endpoint == "" {
				t.Skip(key + " required")
			}
			db, store := databases(t)
			store.PlanContinuations = nil
			var blocks []rag.Section
			for n := range children {
				blocks = append(blocks, rag.Section{Text: fmt.Sprintf("rollback unique segment %d", n), HeadingPath: fmt.Sprint(n)})
			}
			publisher, in, _ := appendRetrievalBlocks(t, db, store, blocks)
			if backend == "qd" {
				in = publishRecallVectors(t, db, store, publisher, in, 1)
			}
			in.TargetNamespace = "bx-projection-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")
			_, target, _, err := projectionIdentity(backend, in)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			forward := httputil.NewSingleHostReverseProxy(u)
			var writes atomic.Int32
			var fail atomic.Bool
			fail.Store(true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut && (strings.Contains(r.URL.Path, "/_doc/") || strings.HasSuffix(r.URL.Path, "/points")) {
					if writes.Add(1) == 2 && fail.Load() {
						http.Error(w, "isolated second write failure", http.StatusServiceUnavailable)
						return
					}
				}
				forward.ServeHTTP(w, r)
			}))
			defer server.Close()
			p := &Projector{DB: db, ES: server.URL, QD: server.URL, HTTP: &http.Client{Timeout: 20 * time.Second}}
			ownedPath := "/" + target
			if backend == "qd" {
				ownedPath = "/collections/" + target
			}
			t.Cleanup(func() {
				// The fixture proxy has already closed when test cleanup runs.
				if err := p.request(context.Background(), "DELETE", strings.TrimRight(endpoint, "/")+ownedPath, nil, backend, nil); err != nil {
					t.Error("owned projection cleanup failed", err)
				}
			})
			ctx := context.Background()
			task := claim(t, store, kind, in)
			computer := &Computer{DB: db, Profile: in.Profile, Project: p.Project}
			if _, err := computer.Compute(ctx, task); err == nil {
				t.Fatal("partial write did not fail")
			} else if err := store.Fail(ctx, task, err, false); err != nil {
				t.Fatal(err)
			}
			var staged int64
			if err := db.Table("ai_artifact_manifests").Where("task_id=?", task.ID).Count(&staged).Error; err != nil || staged != 0 {
				t.Fatal("partial projection acquired a completion manifest", staged, err)
			}
			versions := []string{in.ContentVersionID.String()}
			vector := make([]float32, 1024)
			vector[0] = 1
			if backend == "qd" {
				versions = []string{in.EmbeddingSetID.String()}
			}
			if _, _, err := p.external(ctx, backend, versions, "rollback", vector, 100); err == nil {
				t.Fatal("unpublished physical projection became readable")
			}
			fail.Store(false)
			if err := store.DB.Exec("UPDATE tasks SET not_before=clock_timestamp()-interval '1 second' WHERE id=?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			retry := claim(t, store, kind, in)
			if retry.Generation <= task.Generation {
				t.Fatal("retry did not fence the failed execution")
			}
			body, err := computer.Compute(ctx, retry)
			if err != nil {
				t.Fatal(err)
			}
			result, err := store.SaveResult(ctx, retry, body)
			if err != nil {
				t.Fatal(err)
			}
			publish(t, publisher, retry, result)
			// Receipt replay after AI COMMIT / lost control ACK is idempotent.
			publish(t, publisher, retry, result)
			if rows, name, err := p.external(ctx, backend, versions, "rollback", vector, 100); err != nil || name != target || len(rows) != children {
				t.Fatal("retry lost or duplicated children", name, len(rows), err)
			}
			if _, err := store.SaveResult(ctx, task, body); !errors.Is(err, asynccontrol.ErrLost) {
				t.Fatal("old execution was not fenced after retry publication", err)
			}
			// Tombstone publication hides content immediately; deletion still has
			// to find the physical projection through its independent catalog.
			computePublish(t, publisher, &Computer{DB: db, Profile: in.Profile}, claim(t, store, "ai.object.tombstone", in))
			in.Operation = "delete"
			computePublish(t, publisher, computer, claim(t, store, kind, in))
			var physical int64
			if backend == "es" {
				var response struct{ Count int64 }
				err = p.request(ctx, "GET", server.URL+ownedPath+"/_count", nil, backend, &response)
				physical = response.Count
			} else {
				var response struct{ Result struct{ Count int64 } }
				err = p.request(ctx, "POST", server.URL+ownedPath+"/points/count", map[string]any{"exact": true}, backend, &response)
				physical = response.Result.Count
			}
			if err != nil || physical != 0 {
				t.Fatal("tombstoned physical output survived successful deletion", physical, err)
			}
			var complete int64
			if err := db.Table("ai_published_projections").Where("backend=? AND operation='upsert'", backend).Count(&complete).Error; err != nil || complete != 0 {
				t.Fatal("deleted projection still advertised as complete", complete, err)
			}
			t.Log("partial write -> fenced retry -> accepted publication -> external retrieval -> receipt replay -> tombstone -> physical deletion: PASS")
		})
	}
}
