package aisystem

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"math"
	"net/http"
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
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/11bronx11/bx_yunpan/backend/internal/textsearch"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func readyRetrievalBlocks(t *testing.T, blocks []rag.Section) (*gorm.DB, *asynccontrol.Store, *Publisher, aiprotocol.Input, *corepb.FileRef) {
	t.Helper()
	db, s := databases(t)
	p, in, file := appendRetrievalBlocks(t, db, s, blocks)
	return db, s, p, in, file
}

func appendRetrievalBlocks(t *testing.T, db *gorm.DB, s *asynccontrol.Store, blocks []rag.Section, versions ...uuid.UUID) (*Publisher, aiprotocol.Input, *corepb.FileRef) {
	t.Helper()
	in := testInput()
	seedObject(t, db, in)
	p := &Publisher{DB: db, Control: s}
	task := claim(t, s, "ai.source.prepare", in)
	in.SourceID = uuid.New()
	r, _ := stage(t, db, s, task, in, sourceData{Parsed: ai.ParseResult{Sections: blocks}, Coverage: "complete"}, in.SourceID, int64(len(blocks)))
	publish(t, p, task, r)
	textTask := claim(t, s, "ai.text.build", in)
	if len(versions) == 0 {
		computePublish(t, p, &Computer{DB: db, Profile: in.Profile}, textTask)
	} else {
		chunks, err := rag.BuildParentChild(versions[0], blocks)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := stage(t, db, s, textTask, in, textData{chunks}, versions[0], int64(len(chunks.Parents)+len(chunks.Children)))
		publish(t, p, textTask, result)
	}
	var o Object
	if err := db.First(&o, "object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	in.ContentVersionID = *o.ActiveContentVersionID
	return p, in, &corepb.FileRef{FileId: uuid.NewString(), ObjectId: in.Intent.ObjectID.String(), ContentGeneration: 1, Name: "ranking.txt"}
}

type retrievalProbe struct {
	ai.Provider
	evidence []ai.Evidence
	embedded []string
}

func (p *retrievalProbe) Embeddings(context.Context, []string) ([][]float32, error) {
	v := make([]float32, 1024)
	v[0] = 1
	return [][]float32{v}, nil
}

func (p *retrievalProbe) Answer(ctx context.Context, question string, evidence []ai.Evidence) (ai.AnswerResult, error) {
	p.evidence = append([]ai.Evidence{}, evidence...)
	return p.Provider.Answer(ctx, question, evidence)
}

type contextualEmbeddingProbe struct{ *retrievalProbe }

func (p *contextualEmbeddingProbe) Embeddings(ctx context.Context, texts []string) ([][]float32, error) {
	p.embedded = append(p.embedded, texts...)
	return p.Provider.Embeddings(ctx, texts)
}

type orderingReranker struct{ docs []string }

func (r *orderingReranker) Rerank(_ context.Context, _ string, docs []string) ([]float64, error) {
	r.docs = append([]string{}, docs...)
	scores := make([]float64, len(docs))
	for i := range scores {
		scores[i] = float64(i)
	}
	return scores, nil
}

func retrievalGrant(t *testing.T, mode string, files ...*corepb.FileRef) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g := aiprotocol.ScopeGrant{Scope: aiprotocol.Scope{Mode: mode}}
	if mode == "selected" {
		for _, f := range files {
			g.Scope.FileIDs = append(g.Scope.FileIDs, uuid.MustParse(f.FileId))
		}
	}
	token, err := serviceauth.Sign(key, "core-retrieval-v1", time.Now().Add(time.Minute).Unix(), g)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestHybridTopOneKeepsCrossBranchConsensus(t *testing.T) {
	blocks := []rag.Section{
		{Text: strings.Repeat("rollback ", 10) + "alpha", HeadingPath: "A"},
		{Text: strings.Repeat("rollback ", 3) + "consensus", HeadingPath: "B"},
		{Text: "semantic only", HeadingPath: "C"},
		{Text: "rollback delta", HeadingPath: "D"},
	}
	db, s, p, in, file := readyRetrievalBlocks(t, blocks)
	var children []struct {
		ID          uuid.UUID
		Content     string
		HeadingPath string
	}
	if err := db.Table("ai_published_children").Where("content_version_id=?", in.ContentVersionID).Order("ordinal").Find(&children).Error; err != nil {
		t.Fatal(err)
	}
	vectors := vectorData{}
	for i, ch := range children {
		x := []float32{0.1, 0.9, 1, 0.5}[i]
		v := make([]float32, 1024)
		v[0], v[1] = x, float32(math.Sqrt(1-float64(x*x)))
		vectors.Items = append(vectors.Items, vectorItem{ch.ID, v, aiprotocol.Hash([]byte(embeddingText(in.Profile.Chunker, ch.HeadingPath, ch.Content)))})
	}
	task := claim(t, s, "ai.embedding.generate", in)
	in.EmbeddingSetID = uuid.New()
	r, _ := stage(t, db, s, task, in, vectors, in.EmbeddingSetID, int64(len(children)))
	publish(t, p, task, r)
	q := &Query{DB: db, Core: &scopeClient{file: file}, Profile: in.Profile, Provider: &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}, ScopeLimit: 100}
	var o Object
	if err := db.First(&o, "object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	rows, _, err := q.retrieve(context.Background(), []activeFile{{o, file}}, "rollback", "hybrid", 1)
	if err != nil || len(rows) == 0 || !strings.Contains(rows[0].Content, "consensus") {
		t.Fatalf("top-1 discarded the cross-branch winner: rows=%v err=%v", rows, err)
	}
	want := 2.0 / 62
	if math.Abs(rows[0].Score-want) > 1e-10 {
		t.Fatalf("RRF score=%g, want %g", rows[0].Score, want)
	}
}

func TestAskHonorsRerankCandidateAndEvidenceLimits(t *testing.T) {
	var blocks []rag.Section
	for i := range 6 {
		blocks = append(blocks, rag.Section{Text: fmt.Sprintf("rollback candidate%d", i), HeadingPath: fmt.Sprintf("section%d", i)})
	}
	db, _, _, in, file := readyRetrievalBlocks(t, blocks)
	for _, mode := range []string{"all", "selected"} {
		t.Run(mode, func(t *testing.T) {
			provider := &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
			reranker := &orderingReranker{}
			q := &Query{DB: db, Core: &scopeClient{file: file}, Profile: in.Profile, Provider: provider, Reranker: reranker, ScopeLimit: 100, RerankCandidates: 3, RerankTopK: 2}
			out, err := q.Ask(context.Background(), &pbv2.AskRequest{Grant: retrievalGrant(t, mode, file), Question: "rollback"})
			if err != nil {
				t.Fatal(err)
			}
			if len(reranker.docs) != 3 || len(provider.evidence) != 2 || len(out.Citations) != 2 {
				t.Fatalf("limits ignored: rerank=%d evidence=%d citations=%d", len(reranker.docs), len(provider.evidence), len(out.Citations))
			}
			if !strings.Contains(provider.evidence[0].Content, reranker.docs[2]) {
				t.Fatal("answer did not use the rerank winner first")
			}
		})
	}
}

func TestSummaryOnlyIntentPreservesUnrelatedTerminalFailure(t *testing.T) {
	db, s, p, in, _ := readySmallText(t)
	if err := capability(db, in.Intent, "vector", 1, "failed", "embedding.rejected", uuid.New()); err != nil {
		t.Fatal(err)
	}
	in.Intent.RequestID = uuid.New()
	in.Intent.Targets = []string{"summary"}
	in.Intent.Seqs.Summary++
	computePublish(t, p, &Computer{DB: db, Profile: in.Profile}, claim(t, s, "ai.intent.apply", in))
	var state string
	if err := db.Table("ai_capability_status").Select("latest_build_state").Where("object_id=? AND capability='vector'", in.Intent.ObjectID).Scan(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatal("summary-only request reopened an unscheduled vector build", state)
	}
}

func TestHeadingContextReachesTextEmbeddingRerankAndAnswer(t *testing.T) {
	db, s, p, in, file := readyRetrievalBlocks(t, []rag.Section{{Text: "每次保留三份并异地保存。", HeadingPath: "数据库备份"}})
	ctx := context.Background()
	hits, err := (&Query{DB: db}).pgText(ctx, []string{in.ContentVersionID.String()}, "数据库备份 三份", 10)
	if err != nil || len(hits) != 1 {
		t.Fatal("heading plus body cannot recall its child", hits, err)
	}
	probe := &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
	computePublish(t, p, &Computer{DB: db, Profile: in.Profile, Embedding: &contextualEmbeddingProbe{probe}}, claim(t, s, "ai.embedding.generate", in))
	if len(probe.embedded) != 1 || !strings.Contains(probe.embedded[0], "数据库备份") || !strings.Contains(probe.embedded[0], "三份") {
		t.Fatal("embedding lost section context", probe.embedded)
	}
	reranker := &orderingReranker{}
	q := &Query{DB: db, Core: &scopeClient{file: file}, Provider: probe, Reranker: reranker, Profile: in.Profile, ScopeLimit: 100}
	_, err = q.Ask(ctx, &pbv2.AskRequest{Grant: retrievalGrant(t, "selected", file), Question: "数据库备份 三份"})
	if err != nil || len(reranker.docs) != 1 || len(probe.evidence) != 1 || !strings.Contains(reranker.docs[0], "数据库备份") || !strings.Contains(probe.evidence[0].Content, "数据库备份") {
		t.Fatal("rerank/parent expansion lost section context", err, reranker.docs, probe.evidence)
	}
}

func TestSelectedRAGReservesFilesAndDeduplicatesParentsWithinBudget(t *testing.T) {
	var dominant []rag.Section
	for i := range 6 {
		dominant = append(dominant, rag.Section{Text: strings.Repeat("rollback ", 20) + fmt.Sprintf("dominant%d", i), HeadingPath: fmt.Sprintf("section%d", i)})
	}
	db, s, _, in, first := readyRetrievalBlocks(t, dominant)
	_, _, second := appendRetrievalBlocks(t, db, s, []rag.Section{{Text: "rollback minority-file"}})
	_, _, _ = appendRetrievalBlocks(t, db, s, []rag.Section{{Text: "rollback UNAUTHORIZED_SECRET"}})
	probe := &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
	reranker := &orderingReranker{}
	in.Profile.EnableVectors = false
	q := &Query{DB: db, Core: &parityCore{files: []*corepb.FileRef{first, second}}, Provider: probe, Reranker: reranker, Profile: in.Profile, ScopeLimit: 100, RerankCandidates: 4, RerankTopK: 4}
	out, err := q.Ask(context.Background(), &pbv2.AskRequest{Grant: retrievalGrant(t, "selected", first, second), Question: "rollback"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reranker.docs) != 4 || !strings.Contains(strings.Join(reranker.docs, "\n"), "minority-file") || len(probe.evidence) != 4 || len(out.Coverage.MissingEvidenceFileIds) != 0 {
		t.Fatal("selected file lost its candidate/evidence share", reranker.docs, probe.evidence, out.Coverage)
	}
	count, parents := 0, map[string]bool{}
	for _, citation := range out.Citations {
		if citation.FileId == first.FileId {
			count++
		}
		if parents[citation.ParentId] || citation.FileId != first.FileId && citation.FileId != second.FileId {
			t.Fatal("duplicate parent or scope escaped", citation)
		}
		parents[citation.ParentId] = true
	}
	if count != 3 || out.Retrieval.EvidenceTokens > 12000 || strings.Contains(fmt.Sprint(reranker.docs, probe.evidence), "UNAUTHORIZED_SECRET") {
		t.Fatal("per-file hard cap, token overrun, or unauthorized model input", count, out.Retrieval)
	}
}

func TestHeadingMigrationPreservesSourceAndRebuildsIndex(t *testing.T) {
	db, _, _, in, _ := readyRetrievalBlocks(t, []rag.Section{{Text: "每次保留三份。", HeadingPath: "数据库备份"}})
	for _, direction := range []string{"down", "up"} {
		sql, err := os.ReadFile("../../ai-migrations/000009_heading_context." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(string(sql)).Error; err != nil {
			t.Fatal(err)
		}
		hits, err := (&Query{DB: db}).pgText(context.Background(), []string{in.ContentVersionID.String()}, "数据库备份 三份", 10)
		if err != nil || (len(hits) == 1) != (direction == "up") {
			t.Fatal("migration failed to rebuild lexical data", direction, hits, err)
		}
	}
	var content string
	if err := db.Table("ai_child_chunks").Select("content").Where("content_version_id=?", in.ContentVersionID).Take(&content).Error; err != nil || content != "每次保留三份。" {
		t.Fatal("migration changed the exact child text", content, err)
	}
	for _, legacy := range []string{"parent-child-v1", "parent-child-v2"} {
		profile := in.Profile
		profile.Chunker = legacy
		if !computeProfileCompatible("ai.text.build", profile, in.Profile, "text/plain") || embeddingText(legacy, "heading", "body") != "body" {
			t.Fatal("frozen embedding input changed", legacy)
		}
	}
}

func TestHeadingContextRealElasticsearchProjection(t *testing.T) {
	endpoint := os.Getenv("TEST_SEARCH_ES_ENDPOINT")
	if endpoint == "" {
		t.Skip("dedicated Elasticsearch endpoint required")
	}
	db, _, _, in, _ := readyRetrievalBlocks(t, []rag.Section{{Text: "每次保留三份并异地保存。", HeadingPath: "数据库备份"}})
	in.TargetNamespace = "bx-context-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	p := &Projector{DB: db, ES: endpoint, HTTP: &http.Client{Timeout: 15 * time.Second}}
	_, target, _, err := projectionIdentity("es", in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.request(context.Background(), "DELETE", strings.TrimRight(endpoint, "/")+"/"+target, nil, "es", nil); err != nil {
			t.Error("owned heading projection cleanup failed", err)
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
	for _, query := range []string{"数据库备份 三份", "数据库备份 不存在"} {
		external, name, err := p.external(context.Background(), "es", []string{in.ContentVersionID.String()}, query, nil, 10)
		pg, pgErr := (&Query{DB: db}).pgText(context.Background(), []string{in.ContentVersionID.String()}, query, 10)
		if err != nil || pgErr != nil || name != target || len(external) != len(pg) || (len(external) == 1) != (query == "数据库备份 三份") {
			t.Fatal("PG/ES heading context diverged", query, external, pg, err, pgErr)
		}
	}
}

func TestLegacyProjectionManifestRetainsPublicationPath(t *testing.T) {
	db, _, _, in, _ := readyRetrievalBlocks(t, []rag.Section{{Text: "rollback", HeadingPath: "legacy heading"}})
	var rows []indexedChild
	if err := db.Table("ai_child_chunks").Select("id,parent_id,content,ordinal,heading_path").Where("content_version_id=?", in.ContentVersionID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"text-schema-v1", "text-schema-v2:" + textsearch.Version} {
		id, name, digest, err := projectionIdentityForSchema("es", in, schema)
		if err != nil {
			t.Fatal(err)
		}
		d := projectionData{Backend: "es", TargetID: id, TargetVersion: name, Epoch: in.TargetEpoch, ContentID: in.ContentVersionID, Count: len(rows), Hash: projectionRowsHash(rows, false), ConfigDigest: digest, Operation: "upsert"}
		if err := db.Transaction(func(tx *gorm.DB) error {
			return publishProjection(tx, in, Artifact{Kind: "ai.project.text", Data: jsonValue(d), RowCount: int64(len(rows))})
		}); err != nil {
			t.Fatal("accepted legacy result lost publication path", schema, err)
		}
	}
	if _, _, err := (&Projector{DB: db}).external(context.Background(), "es", []string{in.ContentVersionID.String()}, "rollback", nil, 10); err == nil {
		t.Fatal("legacy projection bypassed current schema proof")
	}
}

func TestAskRerankerFailureRetainsAuthorizedEvidence(t *testing.T) {
	db, _, _, in, file := readySmallText(t)
	probe := &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
	reranker := &observedReranker{}
	q := &Query{DB: db, Core: &scopeClient{file: file}, Provider: probe, Reranker: reranker, Profile: in.Profile, ScopeLimit: 100}
	out, err := q.Ask(context.Background(), &pbv2.AskRequest{Grant: retrievalGrant(t, "selected", file), Question: "rollback"})
	if err != nil || reranker.calls != 1 || len(probe.evidence) != 1 || len(out.GetCitations()) != 1 || !slices.Contains(out.GetRetrieval().GetReasons(), "rerank.unavailable") {
		t.Fatal("rerank failure discarded usable evidence or hid degradation", out, err)
	}
}

func TestGlobalRecallRanksLateScopeBatchBeforeFusion(t *testing.T) {
	db, s := databases(t)
	var files []activeFile
	var last uuid.UUID
	for n := 1; n <= 201; n++ {
		version := uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n))
		text := "rollback"
		if n == 201 {
			text = strings.Repeat("rollback ", 20)
			last = version
		}
		_, in, file := appendRetrievalBlocks(t, db, s, []rag.Section{{Text: text}}, version)
		var o Object
		if err := db.First(&o, "object_id=?", in.Intent.ObjectID).Error; err != nil {
			t.Fatal(err)
		}
		files = append(files, activeFile{o, file})
	}
	rows, _, err := (&Query{DB: db}).retrieve(context.Background(), files, "rollback", "fulltext", 1)
	if err != nil || len(rows) != 1 || rows[0].ContentVersionID != last || math.Abs(rows[0].Score-1.0/61) > 1e-10 {
		t.Fatal("late-batch higher raw score did not become global rank 1", rows, err)
	}
}

func TestAskExpandsRepeatedChildrenOnceAndEnforcesEvidenceBudget(t *testing.T) {
	var blocks []rag.Section
	for n := range 10 {
		blocks = append(blocks, rag.Section{
			Text:        strings.Repeat("rollback beta gamma delta. ", 175),
			HeadingPath: fmt.Sprintf("long-section-%d", n),
		})
	}
	db, _, _, in, file := readyRetrievalBlocks(t, blocks)
	in.Profile.EnableVectors = false
	var parents []struct {
		ID          uuid.UUID
		Content     string
		HeadingPath string
	}
	if err := db.Table("ai_published_parents").Where("content_version_id=?", in.ContentVersionID).Find(&parents).Error; err != nil {
		t.Fatal(err)
	}
	parentText := map[string]string{}
	totalTokens := 0
	for _, p := range parents {
		text := rag.ContextualText(p.HeadingPath, p.Content, 2400)
		parentText[p.ID.String()] = text
		totalTokens += rag.EstimateTokens(text)
	}
	var children int64
	if err := db.Table("ai_published_children").Where("content_version_id=?", in.ContentVersionID).Count(&children).Error; err != nil {
		t.Fatal(err)
	}
	if children <= int64(len(parents))*2 || totalTokens <= 12000 {
		t.Fatal("fixture must hit repeated children and exceed both scope budgets", children, totalTokens)
	}
	for _, tc := range []struct {
		mode   string
		budget int
	}{{"all", 8000}, {"selected", 12000}} {
		t.Run(tc.mode, func(t *testing.T) {
			probe := &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
			reranker := &orderingReranker{}
			q := &Query{DB: db, Core: &scopeClient{file: file}, Provider: probe, Reranker: reranker, Profile: in.Profile, ScopeLimit: 100, RerankCandidates: 100, RerankTopK: 24}
			out, err := q.Ask(context.Background(), &pbv2.AskRequest{Grant: retrievalGrant(t, tc.mode, file), Question: "rollback"})
			if err != nil {
				t.Fatal(err)
			}
			if len(reranker.docs) != int(children) || len(probe.evidence) < 3 || len(probe.evidence) >= len(parents) || len(out.Citations) != len(probe.evidence) {
				t.Fatal("child recall, parent expansion, or budget fixture failed", len(reranker.docs), len(probe.evidence), len(out.Citations))
			}
			seen := map[string]bool{}
			tokens := 0
			for n, c := range out.Citations {
				if seen[c.ParentId] || probe.evidence[n].Content != parentText[c.ParentId] || c.ContentVersionId != in.ContentVersionID.String() {
					t.Fatal("repeated parent or evidence lost its exact published version", c)
				}
				seen[c.ParentId] = true
				tokens += rag.EstimateTokens(probe.evidence[n].Content)
			}
			if tokens != int(out.Retrieval.EvidenceTokens) || tokens > tc.budget || tokens < tc.budget-2400 {
				t.Fatal("evidence budget not enforced or counted incorrectly", tokens, out.Retrieval.EvidenceTokens, tc.budget)
			}
		})
	}
}
