package aisystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func publishRecallVectors(t *testing.T, db *gorm.DB, store *asynccontrol.Store, p *Publisher, in aiprotocol.Input, similarity float32) aiprotocol.Input {
	t.Helper()
	var children []indexedChild
	if err := db.Table("ai_published_children").Where("content_version_id=?", in.ContentVersionID).Find(&children).Error; err != nil {
		t.Fatal(err)
	}
	vectors := vectorData{}
	for _, c := range children {
		v := make([]float32, 1024)
		v[0], v[1] = similarity, float32(math.Sqrt(1-float64(similarity*similarity)))
		vectors.Items = append(vectors.Items, vectorItem{c.ID, v, aiprotocol.Hash([]byte(embeddingText(in.Profile.Chunker, c.HeadingPath, c.Content)))})
	}
	task := claim(t, store, "ai.embedding.generate", in)
	in.EmbeddingSetID = uuid.New()
	result, _ := stage(t, db, store, task, in, vectors, in.EmbeddingSetID, int64(len(children)))
	publish(t, p, task, result)
	return in
}

func TestSingleBranchSearchRetainsChunkRecallBeforeFileGrouping(t *testing.T) {
	for _, backend := range []string{"postgres", "external"} {
		t.Run(backend, func(t *testing.T) {
			if backend == "external" && (os.Getenv("TEST_SEARCH_ES_ENDPOINT") == "" || os.Getenv("TEST_SEARCH_QDRANT_ENDPOINT") == "") {
				t.Skip("dedicated ES and Qdrant endpoints required")
			}
			db, store := databases(t)
			store.PlanContinuations = nil
			var blocks []rag.Section
			for n := range 3 {
				blocks = append(blocks, rag.Section{Text: strings.Repeat("rollback ", 8), HeadingPath: fmt.Sprint(n)})
			}
			p, first, fileA := appendRetrievalBlocks(t, db, store, blocks)
			_, second, fileB := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback unrelated words"}})
			first = publishRecallVectors(t, db, store, p, first, 1)
			second = publishRecallVectors(t, db, store, p, second, 0.5)
			q := &Query{DB: db, Core: &parityCore{files: []*corepb.FileRef{fileA, fileB}}, Profile: first.Profile, Provider: &retrievalProbe{}, ScopeLimit: 100}
			if backend == "external" {
				projector := &Projector{DB: db, ES: os.Getenv("TEST_SEARCH_ES_ENDPOINT"), QD: os.Getenv("TEST_SEARCH_QDRANT_ENDPOINT"), HTTP: &http.Client{Timeout: 15 * time.Second}}
				namespace := "bx-query-" + strings.ReplaceAll(uuid.NewString(), "-", "")
				for _, kind := range []string{"ai.project.text", "ai.project.vector"} {
					for _, in := range []aiprotocol.Input{first, second} {
						in.TargetNamespace = namespace
						computePublish(t, p, &Computer{DB: db, Profile: in.Profile, Project: projector.Project}, claim(t, store, kind, in))
					}
				}
				first.TargetNamespace = namespace
				_, textTarget, _, _ := projectionIdentity("es", first)
				_, vectorTarget, _, _ := projectionIdentity("qd", first)
				t.Cleanup(func() {
					for _, target := range []struct{ url, backend string }{{projector.ES + "/" + textTarget, "es"}, {projector.QD + "/collections/" + vectorTarget, "qd"}} {
						if err := projector.request(context.Background(), "DELETE", target.url, nil, target.backend, nil); err != nil {
							t.Error("owned recall target cleanup failed", err)
						}
					}
				})
				q.Projector = projector
			}
			for _, mode := range []string{"fulltext", "semantic"} {
				t.Run(mode, func(t *testing.T) {
					out, err := q.Search(context.Background(), &pbv2.SearchRequest{Query: "rollback", Mode: mode, Limit: 2})
					if err != nil || len(out.GetHits()) != 2 {
						t.Fatalf("chunk limit hid another matching file: hits=%v err=%v", out.GetHits(), err)
					}
					if backend == "external" && (mode == "fulltext" && out.Retrieval.TextBackend != "elasticsearch" || mode == "semantic" && out.Retrieval.VectorBackend != "qdrant") {
						t.Fatal("external regression silently used PG", out.Retrieval)
					}
				})
			}
		})
	}
}

func TestFulltextPreservesNormalizedPhrasePreference(t *testing.T) {
	db, _, _, in, _ := readyRetrievalBlocks(t, []rag.Section{
		{Text: "ＦＯＯ bar " + strings.Repeat("padding ", 100), HeadingPath: "exact"},
		{Text: strings.Repeat("foo gap bar gap ", 12), HeadingPath: "scattered"},
	})
	var exact struct{ ID uuid.UUID }
	if err := db.Table("ai_published_children").Select("id").Where("content_version_id=? AND heading_path='exact'", in.ContentVersionID).Take(&exact).Error; err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"foo bar", "  ＦＯＯ bar  "} {
		rows, err := (&Query{DB: db}).pgText(context.Background(), []string{in.ContentVersionID.String()}, query, 10)
		if err != nil || len(rows) != 2 || rows[0].ID != exact.ID {
			t.Fatalf("normalized phrase lost its old ranking preference: rows=%v err=%v", rows, err)
		}
	}
	if endpoint := os.Getenv("TEST_SEARCH_ES_ENDPOINT"); endpoint != "" {
		in.TargetNamespace = "bx-phrase-" + strings.ReplaceAll(uuid.NewString(), "-", "")
		p := &Projector{DB: db, ES: endpoint, HTTP: &http.Client{Timeout: 15 * time.Second}}
		_, target, _, err := projectionIdentity("es", in)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := p.request(context.Background(), "DELETE", p.ES+"/"+target, nil, "es", nil); err != nil {
				t.Error("owned phrase target cleanup failed", err)
			}
		})
		data, count, err := p.Project(context.Background(), "ai.project.text", in)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			return publishProjection(tx, in, Artifact{Kind: "ai.project.text", Data: jsonValue(data), RowCount: count})
		}); err != nil {
			t.Fatal(err)
		}
		rows, _, err := p.external(context.Background(), "es", []string{in.ContentVersionID.String()}, "foo bar", nil, 10)
		if err != nil || len(rows) != 2 || rows[0].ID != exact.ID {
			t.Fatalf("ES phrase preference diverged: rows=%v err=%v", rows, err)
		}
	}
}

func TestQueryProviderQuotaPreservesContractAndHybridFallback(t *testing.T) {
	db, store, p, in, file := readySmallText(t)
	publishRecallVectors(t, db, store, p, in, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"AllocationQuota.FreeTierOnly","message":"private quota details"}}`))
	}))
	defer server.Close()
	q := &Query{DB: db, Core: &scopeClient{file: file}, Profile: in.Profile, Provider: ai.NewProvider(config.AI{Provider: "dashscope", BaseURL: server.URL, Dimension: 1024}), ScopeLimit: 100}
	_, err := q.Search(context.Background(), &pbv2.SearchRequest{Query: "rollback", Mode: "semantic"})
	if !errors.Is(ai.FromGRPCError(err), ai.ErrQuota) {
		t.Errorf("semantic quota became a generic outage: %v", err)
	}
	search, err := q.Search(context.Background(), &pbv2.SearchRequest{Query: "rollback", Mode: "hybrid"})
	if err != nil || len(search.GetHits()) != 1 || !slices.Contains(search.GetRetrieval().GetReasons(), "vector.quota_exhausted") {
		t.Errorf("hybrid quota did not retain explicit text fallback: out=%v err=%v", search, err)
	}
	_, err = q.Ask(context.Background(), &pbv2.AskRequest{Grant: retrievalGrant(t, "selected", file), Question: "rollback"})
	if !errors.Is(ai.FromGRPCError(err), ai.ErrQuota) || strings.Contains(err.Error(), "private quota details") {
		t.Errorf("answer quota lost its safe public contract: %v", err)
	}
}

func TestUnscopedExternalHitCannotReachRerankerOrAnswer(t *testing.T) {
	db, store := databases(t)
	store.PlanContinuations = nil
	publisher, in, file := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback authorized evidence"}})
	_, outside, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback PRIVATE_OUTSIDE_SCOPE"}})
	var child struct{ ID uuid.UUID }
	if err := db.Table("ai_published_children").Select("id").Where("content_version_id=?", outside.ContentVersionID).Take(&child).Error; err != nil {
		t.Fatal(err)
	}
	searches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/_search") {
			searches++
			var body struct {
				Query struct {
					Bool struct {
						Filter []struct {
							Terms struct {
								ContentIDs []string `json:"content_id"`
							}
						}
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Query.Bool.Filter) != 1 || fmt.Sprint(body.Query.Bool.Filter[0].Terms.ContentIDs) != fmt.Sprint([]string{in.ContentVersionID.String()}) {
				t.Error("authorized version filter was not pushed down", body, err)
			}
			_, _ = fmt.Fprintf(w, `{"hits":{"hits":[{"_score":100,"_source":{"child_id":%q}}]}}`, child.ID.String())
			return
		}
		_, _ = w.Write([]byte(`{"result":"created","_shards":{"failed":0},"acknowledged":true}`))
	}))
	defer server.Close()
	projector := &Projector{DB: db, ES: server.URL, HTTP: server.Client()}
	for _, input := range []aiprotocol.Input{in, outside} {
		computePublish(t, publisher, &Computer{DB: db, Profile: input.Profile, Project: projector.Project}, claim(t, store, "ai.project.text", input))
	}
	probe := &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
	reranker := &orderingReranker{}
	q := &Query{DB: db, Core: &scopeClient{file: file}, Profile: in.Profile, Provider: probe, Reranker: reranker, Projector: projector, ScopeLimit: 100}
	out, err := q.Ask(context.Background(), &pbv2.AskRequest{Grant: retrievalGrant(t, "selected", file), Question: "rollback"})
	if err != nil || searches != 1 || len(probe.evidence) != 1 || len(reranker.docs) != 1 || !slices.Contains(out.GetRetrieval().GetReasons(), "projection.stale") {
		t.Fatalf("unscoped index hit did not fall back: out=%v searches=%d err=%v", out, searches, err)
	}
	if strings.Contains(reranker.docs[0], "PRIVATE_OUTSIDE_SCOPE") || strings.Contains(probe.evidence[0].Content, "PRIVATE_OUTSIDE_SCOPE") {
		t.Fatal("out-of-scope text reached a model")
	}
	if len(out.Citations) != 1 || out.Citations[0].FileId != file.FileId {
		t.Fatal("out-of-scope citation returned", out.Citations)
	}
}
