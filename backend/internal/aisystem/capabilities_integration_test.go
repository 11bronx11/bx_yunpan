package aisystem

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
)

type observedProvider struct {
	ai.Provider
	failEmbedding, failSummary     bool
	embeddings, summaries, answers int
	inputs                         []string
}

func (p *observedProvider) Embeddings(ctx context.Context, texts []string) ([][]float32, error) {
	p.embeddings++
	if p.failEmbedding {
		return nil, errors.New("fixture embedding failure")
	}
	return p.Provider.Embeddings(ctx, texts)
}

func (p *observedProvider) Enrich(ctx context.Context, text string) (ai.Insight, error) {
	p.summaries++
	p.inputs = append(p.inputs, text)
	if p.failSummary {
		return ai.Insight{}, errors.New("fixture summary failure")
	}
	return p.Provider.Enrich(ctx, text)
}

func (p *observedProvider) Answer(ctx context.Context, text string, e []ai.Evidence, options ai.AnswerOptions) (ai.AnswerResult, error) {
	p.answers++
	return p.Provider.Answer(ctx, text, e, options)
}

type scopeClient struct {
	corepb.CoreReadServiceClient
	file    *corepb.FileRef
	invalid bool
}

func (c *scopeClient) BatchGetFileRefs(context.Context, *corepb.FileRefsRequest, ...grpc.CallOption) (*corepb.FileRefsResponse, error) {
	return &corepb.FileRefsResponse{Files: []*corepb.FileRef{c.file}}, nil
}

func (c *scopeClient) OpenRetrievalScope(context.Context, *corepb.OpenScopeRequest, ...grpc.CallOption) (*corepb.ScopeResponse, error) {
	return &corepb.ScopeResponse{SnapshotId: "fixture"}, nil
}

func (c *scopeClient) StreamScopeMembers(context.Context, *corepb.ScopeMembersRequest, ...grpc.CallOption) (*corepb.ScopeMembersResponse, error) {
	return &corepb.ScopeMembersResponse{Files: []*corepb.FileRef{c.file}}, nil
}

func (c *scopeClient) ValidateEvidence(context.Context, *corepb.ValidateEvidenceRequest, ...grpc.CallOption) (*corepb.ValidateEvidenceResponse, error) {
	if c.invalid {
		return nil, status.Error(codes.Aborted, "ai.scope_changed")
	}
	return &corepb.ValidateEvidenceResponse{}, nil
}

type observedReranker struct{ calls int }

func (r *observedReranker) Rerank(context.Context, string, []string) ([]float64, error) {
	r.calls++
	return nil, errors.New("fixture")
}

func readyText(t *testing.T) (*gorm.DB, *asynccontrol.Store, *Publisher, aiprotocol.Input, *corepb.FileRef) {
	t.Helper()
	db, s := databases(t)
	in := testInput()
	seedObject(t, db, in)
	p := &Publisher{DB: db, Control: s}
	source := uuid.New()
	task := claim(t, s, "ai.source.prepare", in)
	sections := []rag.Section{{Text: strings.Repeat("rollback alpha 保留数据，检查权限。\n", 2200), Kind: "text"}, {Text: "全文末尾TAIL_MARKER回滚结论", Kind: "text"}}
	r, _ := stage(t, db, s, task, in, sourceData{Parsed: ai.ParseResult{ParserID: "text", ParserVersion: "v1", Sections: sections}, Coverage: "complete"}, source, int64(len(sections)))
	publish(t, p, task, r)
	in.SourceID = source
	task = claim(t, s, "ai.text.build", in)
	computer := Computer{DB: db, Profile: in.Profile}
	body, err := computer.Compute(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.SaveResult(context.Background(), task, body)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, p, task, r)
	out, _ := decode[aiprotocol.Output](body)
	in.ContentVersionID = out.OutputID
	file := &corepb.FileRef{FileId: uuid.NewString(), ObjectId: in.Intent.ObjectID.String(), ContentGeneration: 1, Name: "alpha.txt"}
	return db, s, p, in, file
}

func computePublish(t *testing.T, p *Publisher, c *Computer, task asynccontrol.Task) {
	t.Helper()
	ctx := context.Background()
	body, err := c.Compute(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Control.SaveResult(ctx, task, body)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, p, task, r)
}

func TestIndependentFailuresFullTextAskAndSummaryCoverage(t *testing.T) {
	db, s, p, in, file := readyText(t)
	ctx := context.Background()
	provider := &observedProvider{Provider: ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024}), failEmbedding: true, failSummary: true}
	computer := &Computer{DB: db, Profile: in.Profile, Embedding: provider, Summary: provider, SummaryCalls: 128}
	for _, kind := range []string{"ai.embedding.generate", "ai.summary.generate"} {
		taskInput := in
		if kind == "ai.summary.generate" {
			taskInput.ContentVersionID = uuid.Nil
		}
		task := claim(t, s, kind, taskInput)
		if _, err := computer.Compute(ctx, task); err == nil {
			t.Fatal("provider failure ignored", kind)
		}
		if err := s.Fail(ctx, task, errors.New("fixture"), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Recover(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// Failure snapshots are independently accepted and published, without
	// removing the text version that users can already query.
	for range 2 {
		task, ok, err := s.Reserve(ctx, "ai-content")
		if err != nil || !ok {
			t.Fatal("missing failure snapshot", err)
		}
		task, ok, err = s.Claim(ctx, task.Notice())
		if err != nil || !ok {
			t.Fatal(err)
		}
		if task.Kind != "ai.capability.finish" {
			t.Fatal(task.Kind)
		}
		computePublish(t, p, computer, task)
	}
	q := &Query{DB: db, Core: &scopeClient{file: file}, Provider: provider, Profile: in.Profile, ScopeLimit: 100}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	g := aiprotocol.ScopeGrant{Scope: aiprotocol.Scope{Mode: "selected", FileIDs: []uuid.UUID{uuid.MustParse(file.FileId)}}}
	grant, err := serviceauth.Sign(key, "core-retrieval-v1", time.Now().Add(time.Minute).Unix(), g)
	if err != nil {
		t.Fatal(err)
	}
	before := provider.embeddings
	search, err := q.Search(ctx, &pbv2.SearchRequest{Grant: grant, Query: "rollback", Mode: "fulltext"})
	if err != nil || len(search.Hits) != 1 {
		t.Fatal("text search blocked by failures", err)
	}
	if provider.embeddings != before {
		t.Fatal("fulltext called embedding")
	}
	answer, err := q.Ask(ctx, &pbv2.AskRequest{Grant: grant, Question: "rollback"})
	if err != nil || len(answer.Citations) < 3 || !answer.Retrieval.Degraded {
		t.Fatal("text-only single-file ask", err, answer)
	}
	caps, err := q.capabilities(ctx, &pbv2.ObjectRef{ObjectId: file.ObjectId, ContentGeneration: 1, MimeType: "text/plain"})
	if err != nil || !caps.Askable || caps.Vector.Available || caps.Summary.Available {
		t.Fatal("capability independence", err, caps)
	}
	provider.failEmbedding = false
	in.Intent.Seqs.Vector = 2
	in.Intent.RequestID = uuid.New()
	in.Intent.Targets = []string{"vectors"}
	if err := db.Exec("UPDATE ai_objects SET desired_vector_seq=2 WHERE object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	computePublish(t, p, computer, claim(t, s, "ai.embedding.generate", in))
	caps, err = q.capabilities(ctx, &pbv2.ObjectRef{ObjectId: file.ObjectId, ContentGeneration: 1, MimeType: "text/plain"})
	if err != nil || !caps.Vector.Available || caps.Summary.Available {
		t.Fatal("summary failure blocked vectors", err, caps)
	}
	result, err := q.Search(ctx, &pbv2.SearchRequest{Grant: grant, Query: "rollback", Mode: "semantic"})
	if err != nil || len(result.Hits) == 0 {
		t.Fatal("PG vector query failed", err)
	}
	q.Profile.EmbeddingRevision = "upgrade-v2"
	if _, err := q.Search(ctx, &pbv2.SearchRequest{Grant: grant, Query: "rollback", Mode: "semantic"}); status.Code(err) != codes.Unavailable {
		t.Fatal("incompatible query model accepted", err)
	}
	fallback, err := q.Search(ctx, &pbv2.SearchRequest{Grant: grant, Query: "rollback", Mode: "hybrid"})
	if err != nil || len(fallback.Hits) == 0 || !fallback.Retrieval.Degraded {
		t.Fatal("model upgrade destroyed text fallback", err)
	}
	q.Profile = in.Profile
	// A summary-only retry uses canonical blocks including the end of a long
	// document and never calls embedding. It has a new explicit request sequence.
	in.Intent.RequestID = uuid.New()
	in.Intent.Targets = []string{"summary"}
	in.Intent.Seqs.Summary = 2
	if err := db.Exec("UPDATE ai_objects SET desired_summary_seq=2 WHERE object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	provider.failSummary = false
	before = provider.embeddings
	computePublish(t, p, computer, claim(t, s, "ai.summary.generate", in))
	if provider.embeddings != before || !strings.Contains(strings.Join(provider.inputs, "\n"), "TAIL_MARKER") {
		t.Fatal("summary omitted tail or recomputed vectors")
	}
	// A newer failure observation may mark freshness, but an old failure must
	// never overwrite the successful publication's status.
	if err := capability(db, in.Intent, "summary", 1, "failed", "old", uuid.New()); err != nil {
		t.Fatal(err)
	}
	caps, err = q.capabilities(ctx, &pbv2.ObjectRef{ObjectId: file.ObjectId, ContentGeneration: 1, MimeType: "text/plain"})
	if err != nil || !caps.Summary.Available || caps.Summary.LatestBuild.State != "succeeded" {
		t.Fatal("late failure overwrote success", err, caps)
	}
	// No candidate may reach either external model after a failed core check.
	reranker := &observedReranker{}
	q.Reranker = reranker
	q.Core = &scopeClient{file: file, invalid: true}
	before = provider.answers
	_, err = q.Ask(ctx, &pbv2.AskRequest{Grant: grant, Question: "rollback"})
	if status.Code(err) != codes.Aborted || reranker.calls != 0 || provider.answers != before {
		t.Fatal("unauthorized evidence reached a model", err)
	}
}

func TestMaintenanceIdentityAndGraceProtectAcceptedArtifacts(t *testing.T) {
	db, s, p, in, _ := readyText(t)
	ctx := context.Background()
	// Maintenance input intentionally has no provider/profile authorization.
	gcIn := aiprotocol.Input{Intent: aiprotocol.Intent{ObjectID: in.Intent.ObjectID, ContentGeneration: 1, ContentHash: in.Intent.ContentHash, Size: in.Intent.Size}}
	task := claim(t, s, "ai.artifact.gc", gcIn)
	computer := &Computer{DB: db, Profile: in.Profile}
	computePublish(t, p, computer, task)
	var active int64
	_ = db.Table("ai_child_chunks").Count(&active)
	if active == 0 {
		t.Fatal("GC deleted active content")
	}
	var continuations []asynccontrol.Continuation
	if err := s.DB.Where("parent_task_id=?", task.ID).Find(&continuations).Error; err != nil || len(continuations) != 3 {
		t.Fatal("missing bounded GC successor", err)
	}
	var next asynccontrol.Spec
	for _, row := range continuations {
		var candidate asynccontrol.Spec
		_ = json.Unmarshal(row.Spec, &candidate)
		if candidate.Kind == "ai.artifact.gc" {
			next = candidate
		}
	}

	var frozen aiprotocol.Input
	_ = json.Unmarshal(next.Input, &frozen)
	if frozen.Intent.ObjectID != in.Intent.ObjectID || !next.Deadline.After(task.Deadline) {
		t.Fatal("GC inherited expired processing deadline")
	}
	ids, safe, err := p.collectableArtifacts(ctx, in.Intent)
	if err != nil || len(ids) != 0 || safe {
		t.Fatal("pending computations were not protected", err)
	}
}

func TestRetiredUnacceptedProjectionIsRememberedUntilPhysicalDeletion(t *testing.T) {
	db, _, p, in, _ := readyText(t)
	// A projector may have written external bytes before losing its lease. Its
	// artifact has no published projection state and must not be silently lost.
	target := uuid.New()
	a := Artifact{ArtifactID: uuid.New(), TaskID: uuid.New(), Generation: 1, ObjectID: in.Intent.ObjectID, ContentGeneration: 1, Kind: "ai.project.text", Complete: true, Data: jsonValue(projectionData{Backend: "es", TargetID: target, TargetVersion: "fixture-retired", ContentID: in.ContentVersionID, Operation: "upsert"}), CreatedAt: time.Now().Add(-10 * 24 * time.Hour)}
	if err := db.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	clean := func() {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			_, err := p.cleanArtifacts(tx, in.Intent, []uuid.UUID{a.ArtifactID}, true)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	clean()
	var count int64
	_ = db.Model(&Artifact{}).Where("artifact_id=?", a.ArtifactID).Count(&count)
	if count != 1 {
		t.Fatal("active-version cleanup proof was discarded")
	}
	if err := db.Exec("UPDATE ai_objects SET active_content_version_id=NULL WHERE object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE ai_content_versions SET retired_at=clock_timestamp()-interval '9 days' WHERE id=?", in.ContentVersionID).Error; err != nil {
		t.Fatal(err)
	}
	clean()
	_ = db.Table("ai_orphan_projections").Count(&count)
	if count != 1 {
		t.Fatal("retired physical output lost its cleanup record")
	}
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/fixture-retired/_delete_by_query" {
			t.Error("unexpected cleanup target", r.URL.Path)
		}
		// An absent physical target is an idempotently completed deletion.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	in.Operation = "retire"
	d, _, err := (&Projector{DB: db, ES: server.URL, HTTP: server.Client()}).projectDelete(context.Background(), "es", in)
	if err != nil || !called {
		t.Fatal("orphan cleanup did not reach its independent projector", err)
	}
	if err := publishDelete(db, in, Artifact{Data: jsonValue(d)}); err != nil {
		t.Fatal(err)
	}
	_ = db.Table("ai_orphan_projections").Count(&count)
	if count != 0 {
		t.Fatal("published deletion retained obsolete cleanup work")
	}
}
