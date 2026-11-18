package aisystem

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type parityCore struct {
	scopeClient
	object *corepb.ProcessingResponse
	files  []*corepb.FileRef
}

func (c *parityCore) GetObjectForProcessing(context.Context, *corepb.ProcessingRequest, ...grpc.CallOption) (*corepb.ProcessingResponse, error) {
	return c.object, nil
}

func (c *parityCore) StreamScopeMembers(context.Context, *corepb.ScopeMembersRequest, ...grpc.CallOption) (*corepb.ScopeMembersResponse, error) {
	return &corepb.ScopeMembersResponse{Files: c.files}, nil
}

func TestVisionSourcePublishesTextSummaryAndVectors(t *testing.T) {
	ctx := context.Background()
	db, store := databases(t)
	in := testInput()
	in.Intent.MediaType = "image/png"
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	in.Intent.ContentHash = aiprotocol.Hash(imageBytes.Bytes())
	in.Intent.Size = int64(imageBytes.Len())
	seedObject(t, db, in)
	var visionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = w.Write(imageBytes.Bytes())
			return
		}
		visionCalls.Add(1)
		var req struct {
			Model    string
			Messages []struct{ Content []struct{ Type string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "vision-fixture" || len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
			t.Error("vision multimodal request missing", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"红色工厂设备，序列号 BX7319，项目预算审批图"}}]}`))
	}))
	defer server.Close()
	provider := &observedProvider{Provider: ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})}
	c := &Computer{DB: db, Core: &parityCore{object: &corepb.ProcessingResponse{ReadUrl: server.URL, Size: in.Intent.Size, Sha256: in.Intent.ContentHash}}, Vision: ai.NewProvider(config.AI{Provider: "dashscope", BaseURL: server.URL, VisionModel: "vision-fixture", Dimension: 1024}), Embedding: provider, Summary: provider, Profile: in.Profile, HTTP: server.Client(), SummaryCalls: 128}
	p := &Publisher{DB: db, Control: store}
	sourceTask := claim(t, store, "ai.source.prepare", in)
	body, err := c.Compute(ctx, sourceTask)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.SaveResult(ctx, sourceTask, body)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, p, sourceTask, result)
	out, _ := decode[aiprotocol.Output](body)
	in.SourceID = out.OutputID
	var source struct{ ParserID, VisionModelVersion string }
	if err := db.Table("ai_published_sources").Where("id=?", in.SourceID).Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	if source.ParserID != "cloud-vision" || source.VisionModelVersion != "vision-fixture" || visionCalls.Load() != 1 {
		t.Fatal("vision provenance lost", source, visionCalls.Load())
	}
	textTask := claim(t, store, "ai.text.build", in)
	body, err = c.Compute(ctx, textTask)
	if err != nil {
		t.Fatal(err)
	}
	result, err = store.SaveResult(ctx, textTask, body)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, p, textTask, result)
	out, _ = decode[aiprotocol.Output](body)
	in.ContentVersionID = out.OutputID
	computePublish(t, p, c, claim(t, store, "ai.embedding.generate", in))
	summaryIn := in
	summaryIn.ContentVersionID = uuid.Nil
	computePublish(t, p, c, claim(t, store, "ai.summary.generate", summaryIn))
	q := &Query{DB: db, Profile: in.Profile}
	caps, err := q.capabilities(ctx, &pbv2.ObjectRef{ObjectId: in.Intent.ObjectID.String(), ContentGeneration: 1, MimeType: "image/png"})
	if err != nil || !caps.Askable || !caps.Vector.Available || !caps.Summary.Available {
		t.Fatal("image chain incomplete", caps, err)
	}
	file := &corepb.FileRef{FileId: uuid.NewString(), ObjectId: in.Intent.ObjectID.String(), ContentGeneration: 1, MimeType: "image/png"}
	q.Core = &scopeClient{file: file}
	for _, thumbnail := range []string{"pending", "available"} {
		file.ThumbnailState = thumbnail
		overview, e := q.GetOverview(ctx, &pbv2.GetOverviewRequest{FileId: file.FileId})
		if e != nil || overview.Summary == "" || overview.PreviewState != thumbnail {
			t.Fatal("image summary or thumbnail availability lost", overview, e)
		}
	}
	hits, err := q.pgText(ctx, []string{in.ContentVersionID.String()}, "项目预算", 10)
	if err != nil || len(hits) != 1 || provider.embeddings != 1 || provider.summaries != 1 {
		t.Fatal("image text not searchable or models repeated", err, hits)
	}
}

func TestVisionIsPlannedAndParserStopsConverge(t *testing.T) {
	for _, mime := range []string{"image/png", "image/jpeg", "IMAGE/PNG; charset=binary"} {
		in := testInput()
		in.Intent.MediaType = mime
		task := asynccontrol.Task{ID: uuid.New(), Kind: "ai.intent.apply", Generation: 1, InputDigest: strings.Repeat("a", 64), Input: jsonValue(in)}
		out := aiprotocol.Output{Version: 2, TaskID: task.ID, Generation: 1, InputDigest: task.InputDigest, ArtifactID: uuid.New(), ArtifactHash: strings.Repeat("b", 64), OutputID: uuid.New(), ObjectID: in.Intent.ObjectID, ContentGeneration: 1, Complete: true}
		specs, err := (&Planner{}).Continuations(task, asynccontrol.Result{Body: jsonValue(out)})
		if err != nil || !containsKind(specs, "ai.source.prepare") {
			t.Fatal("image skipped", mime, specs, err)
		}
	}
	for _, err := range []error{&ai.ParseError{Unsupported: true}, &ai.ParseError{ResourceLimit: true}, &ai.ParseError{Permanent: true}} {
		if !IsPermanentComputeError(err) {
			t.Fatal("permanent parser failure retried", err)
		}
	}
	if IsPermanentComputeError(&ai.ParseError{Retryable: true}) {
		t.Fatal("transient parser failure stopped")
	}
	// Persist the source stop observations and check downstream state, not just planning.
	db, store := databases(t)
	in := testInput()
	seedObject(t, db, in)
	task := asynccontrol.Task{ID: uuid.New(), Kind: "ai.source.prepare", Input: jsonValue(in)}
	specs, err := (&Planner{}).Failure(task)
	if err != nil || len(specs) != 4 {
		t.Fatal(specs, err)
	}
	for _, sp := range specs {
		computePublish(t, &Publisher{DB: db, Control: store}, &Computer{DB: db, Profile: in.Profile}, claim(t, store, sp.Kind, mustInput(t, sp.Input)))
	}
	caps, err := (&Query{DB: db, Profile: in.Profile}).capabilities(context.Background(), &pbv2.ObjectRef{ObjectId: in.Intent.ObjectID.String(), ContentGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, cap := range []*pbv2.Capability{caps.Source, caps.Text, caps.Vector, caps.Summary} {
		if cap.LatestBuild.State != "failed" {
			t.Fatal("stranded pending capability", cap)
		}
	}
}

func mustInput(t *testing.T, b []byte) aiprotocol.Input {
	t.Helper()
	in, e := decode[aiprotocol.Input](b)
	if e != nil {
		t.Fatal(e)
	}
	return in
}

func containsKind(specs []asynccontrol.Spec, kind string) bool {
	for _, s := range specs {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

type brokenEmbeddingCache struct{}

func (brokenEmbeddingCache) Get(context.Context, string) ([]float32, bool, error) {
	return nil, false, errors.New("cache unavailable")
}

func (brokenEmbeddingCache) Set(context.Context, string, []float32) error {
	return errors.New("cache unavailable")
}

func TestQueryEmbeddingCacheAndDisabledVectorBoundary(t *testing.T) {
	provider := &observedProvider{Provider: ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})}
	q := &Query{Provider: provider, Profile: testInput().Profile, EmbeddingCache: platformcache.NewEmbeddingStore(nil, time.Minute, 16)}
	for i := 0; i < 2; i++ {
		if _, e := q.embedQuery(context.Background(), "中文预算"); e != nil {
			t.Fatal(e)
		}
	}
	if provider.embeddings != 1 {
		t.Fatal("cache not used", provider.embeddings)
	}
	q.Profile.EmbeddingRevision = "new-revision"
	_, _ = q.embedQuery(context.Background(), "中文预算")
	if provider.embeddings != 2 {
		t.Fatal("cross revision vector reused")
	}
	q.EmbeddingCache = brokenEmbeddingCache{}
	if _, e := q.embedQuery(context.Background(), "中文预算"); e != nil {
		t.Fatal("cache failure became query failure", e)
	}
	db, store, p, in, file := readySmallText(t)
	c := &Computer{DB: db, Profile: in.Profile, Embedding: provider}
	computePublish(t, p, c, claim(t, store, "ai.embedding.generate", in))
	q = &Query{DB: db, Core: &scopeClient{file: file}, Provider: provider, Profile: in.Profile}
	q.Profile.EnableVectors = false
	before := provider.embeddings
	if _, e := q.Search(context.Background(), &pbv2.SearchRequest{Query: "rollback", Mode: "semantic"}); status.Code(e) != codes.Unavailable {
		t.Fatal("disabled semantic accepted", e)
	}
	if _, e := q.Search(context.Background(), &pbv2.SearchRequest{Query: "rollback", Mode: "hybrid"}); e != nil {
		t.Fatal("disabled vectors broke text fallback", e)
	}
	if provider.embeddings != before {
		t.Fatal("disabled vectors called model")
	}
	caps, e := q.capabilities(context.Background(), &pbv2.ObjectRef{ObjectId: uuid.NewString(), ContentGeneration: 1, MimeType: "text/plain"})
	if e != nil || caps.Vector.Supported {
		t.Fatal("disabled vectors declared supported before intent", caps, e)
	}
}

func TestLogicalAliasesRemainSearchableAndCitable(t *testing.T) {
	db, _, _, in, file := readySmallText(t)
	alias := proto.Clone(file).(*corepb.FileRef)
	alias.FileId = uuid.NewString()
	alias.Name = "authorized-copy.txt"
	core := &parityCore{scopeClient: scopeClient{file: file}, files: []*corepb.FileRef{file, alias}}
	q := &Query{DB: db, Core: core, Profile: in.Profile, Provider: ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})}
	result, e := q.Search(context.Background(), &pbv2.SearchRequest{Query: "rollback", Mode: "fulltext"})
	if e != nil || len(result.Hits) != 2 {
		t.Fatal("logical file alias lost", result, e)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	first, _ := uuid.Parse(file.FileId)
	second, _ := uuid.Parse(alias.FileId)
	grant, _ := serviceauth.Sign(key, "core-retrieval-v1", time.Now().Add(time.Minute).Unix(), aiprotocol.ScopeGrant{Scope: aiprotocol.Scope{Mode: "selected", FileIDs: []uuid.UUID{first, second}}})
	answer, e := q.Ask(context.Background(), &pbv2.AskRequest{Grant: grant, Question: "rollback"})
	if e != nil || len(answer.Citations) != 2 || len(answer.Coverage.MissingEvidenceFileIds) != 0 {
		t.Fatal("selected aliases lost their citations", answer, e)
	}
}

func TestProjectionWriteFlagsAreFrozen(t *testing.T) {
	for _, backends := range [][]string{{"postgres"}, {"postgres", "es_qdrant"}, nil} {
		in := testInput()
		in.ProjectionBackends = backends
		task := asynccontrol.Task{ID: uuid.New(), Kind: "ai.text.build", Generation: 1, InputDigest: strings.Repeat("a", 64), Input: jsonValue(in)}
		out := aiprotocol.Output{Version: 2, TaskID: task.ID, Generation: 1, InputDigest: task.InputDigest, ArtifactID: uuid.New(), ArtifactHash: strings.Repeat("b", 64), OutputID: uuid.New(), ObjectID: in.Intent.ObjectID, ContentGeneration: 1, Complete: true}
		specs, e := (&Planner{}).Continuations(task, asynccontrol.Result{Body: jsonValue(out)})
		want := len(backends) != 1
		if e != nil || containsKind(specs, "ai.project.text") != want || !containsKind(specs, "ai.embedding.generate") {
			t.Fatal("projection flags changed vector preparation", backends, specs, e)
		}
	}
}

type boundedEvidenceCore struct {
	scopeClient
	calls int
}

func (c *boundedEvidenceCore) ValidateEvidence(_ context.Context, r *corepb.ValidateEvidenceRequest, _ ...grpc.CallOption) (*corepb.ValidateEvidenceResponse, error) {
	c.calls++
	if len(r.Refs) > 240 {
		return nil, status.Error(codes.InvalidArgument, "too many refs")
	}
	return &corepb.ValidateEvidenceResponse{}, nil
}

func TestManyLogicalAliasesUseBoundedEvidenceBatches(t *testing.T) {
	core := &boundedEvidenceCore{}
	aliases := []*corepb.FileRef{}
	for range 501 {
		aliases = append(aliases, &corepb.FileRef{FileId: uuid.NewString()})
	}
	q := &Query{Core: core}
	if e := q.validate(context.Background(), "grant", []candidate{{Aliases: aliases}}); e != nil || core.calls != 3 {
		t.Fatal("alias permission batch overflow", e, core.calls)
	}
}

func TestShadowReadPreservesPGAndNeverDuplicatesEmbedding(t *testing.T) {
	db, store, p, in, file := readySmallText(t)
	var children []uuid.UUID
	if e := db.Table("ai_published_children").Where("content_version_id=?", in.ContentVersionID).Pluck("id", &children).Error; e != nil {
		t.Fatal(e)
	}
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/_search") {
			if fail.Load() {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": []any{map[string]any{"_score": 1, "_source": map[string]any{"child_id": children[0].String()}}}}})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	projector := &Projector{DB: db, ES: server.URL, HTTP: server.Client()}
	c := &Computer{DB: db, Profile: in.Profile, Project: projector.Project}
	computePublish(t, p, c, claim(t, store, "ai.project.text", in))
	recorder := ai.NewMemoryShadowRecorder(4)
	provider := &observedProvider{Provider: ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})}
	q := &Query{DB: db, Core: &scopeClient{file: file}, Profile: in.Profile, Provider: provider, ShadowProjector: projector, ShadowRecorder: recorder, ShadowSampleRate: 1}
	for _, unavailable := range []bool{false, true} {
		fail.Store(unavailable)
		result, e := q.Search(context.Background(), &pbv2.SearchRequest{Query: "rollback", Mode: "fulltext"})
		if e != nil || len(result.Hits) != 1 || result.Retrieval.TextBackend != "postgres" {
			t.Fatal("shadow changed primary result", e, result)
		}
	}
	entries := recorder.Snapshot()
	if len(entries) != 2 || entries[0].OverlapCount != 1 || entries[1].ExternalError == "" || provider.embeddings != 0 {
		t.Fatal("shadow lost aggregate comparison or called model", entries, provider.embeddings)
	}
}

func TestTableSearchKeepsTailAndRowProvenance(t *testing.T) {
	db, store := databases(t)
	in := testInput()
	in.Profile.Chunker = rag.ParentChildVersion
	seedObject(t, db, in)
	var text strings.Builder
	text.WriteString("| 设备编号 | 项目预算 |\n| --- | --- |\n")
	for row := 2; row <= 81; row++ {
		fmt.Fprintf(&text, "| BX%04d | %s |\n", row, strings.Repeat("中", 90))
	}
	source := uuid.New()
	task := claim(t, store, "ai.source.prepare", in)
	result, _ := stage(t, db, store, task, in, sourceData{Parsed: ai.ParseResult{ParserID: "table-fixture", ParserVersion: "v1", Sections: []rag.Section{{Text: text.String(), Kind: rag.KindTable, HasHeader: true, SheetName: "预算表", RowStart: 2, RowEnd: 81, ColumnStart: 1, ColumnEnd: 2}}}, Coverage: "complete"}, source, 1)
	p := &Publisher{DB: db, Control: store}
	publish(t, p, task, result)
	in.SourceID = source
	task = claim(t, store, "ai.text.build", in)
	c := &Computer{DB: db, Profile: in.Profile}
	body, e := c.Compute(context.Background(), task)
	if e != nil {
		t.Fatal(e)
	}
	result, e = store.SaveResult(context.Background(), task, body)
	if e != nil {
		t.Fatal(e)
	}
	publish(t, p, task, result)
	file := &corepb.FileRef{FileId: uuid.NewString(), ObjectId: in.Intent.ObjectID.String(), ContentGeneration: 1, MimeType: "text/csv"}
	q := &Query{DB: db, Core: &scopeClient{file: file}, Profile: in.Profile}
	search, e := q.Search(context.Background(), &pbv2.SearchRequest{Query: "BX0081", Mode: "fulltext"})
	if e != nil || len(search.Hits) != 1 || len(search.Hits[0].Citations) == 0 {
		t.Fatal("table tail not searchable", e, search)
	}
	for _, cite := range search.Hits[0].Citations {
		location := cite.SourceLocation
		if location == nil || location.SheetName != "预算表" || location.RowStart > 81 || location.RowEnd < 81 || location.ColumnEnd != 2 || cite.SourceVersionId != source.String() {
			t.Fatal("table citation lost row/version provenance", cite)
		}
	}
}

type recordedEmbeddingCache struct {
	ai.QueryEmbeddingCache
	key string
}

func (c *recordedEmbeddingCache) Set(ctx context.Context, key string, v []float32) error {
	c.key = key
	return c.QueryEmbeddingCache.Set(ctx, key, v)
}

func TestQueryEmbeddingCacheRealRedisAcrossInstances(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR required")
	}
	client := platformcache.NewRedisClient(addr, "", 0)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if e := client.Ping(ctx).Err(); e != nil {
		t.Fatal("test Redis unavailable")
	}
	first := &recordedEmbeddingCache{QueryEmbeddingCache: platformcache.NewEmbeddingStore(client, time.Minute, 8)}
	defer func() {
		if first.key != "" {
			_ = client.Del(context.Background(), first.key).Err()
		}
	}()
	provider := &observedProvider{Provider: ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})}
	q := &Query{Provider: provider, Profile: testInput().Profile, EmbeddingCache: first}
	query := "parity-cache-" + uuid.NewString()
	if _, e := q.embedQuery(ctx, query); e != nil {
		t.Fatal(e)
	}
	q.EmbeddingCache = platformcache.NewEmbeddingStore(client, time.Minute, 8)
	if _, e := q.embedQuery(ctx, query); e != nil || provider.embeddings != 1 {
		t.Fatal("real Redis cache missed across query instances", e, provider.embeddings)
	}
}
