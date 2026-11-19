package aisystem

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// Exercise every content stage after the generic artifact table has actually
// been archived, including publication rollback and incompatible vector removal.
func TestDirectContractedVisionReplacementAndQueries(t *testing.T) {
	db, store := directDatabases(t)
	ctx := context.Background()
	in := testInput()
	in.Intent.MediaType = "image/png"
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	in.Intent.Size, in.Intent.ContentHash = int64(picture.Len()), aiprotocol.Hash(picture.Bytes())
	publisher := &Publisher{DB: db, Control: store}
	applyDirectIntent(t, publisher, in)
	contract, err := os.ReadFile("../../ai-contract-migrations/000001_archive_artifacts.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&DirectMigrator{DB: db, Control: store}).Archive(ctx, string(contract)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP SCHEMA ai_v2_archive CASCADE") })

	var visionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write(picture.Bytes())
			return
		}
		var request struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "direct-vision" {
			t.Error("invalid vision request", err)
		}
		visionCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"rollback BX7319 图片设备保留旧版本，审批预算。"}}]}`))
	}))
	defer server.Close()
	in.Profile.VisionProvider, in.Profile.VisionModel = "dashscope", "direct-vision"
	file := &corepb.FileRef{FileId: uuid.NewString(), ObjectId: in.Intent.ObjectID.String(), ContentGeneration: 1, MimeType: "image/png", Name: "direct.png"}
	core := &parityCore{scopeClient: scopeClient{file: file}, files: []*corepb.FileRef{file}, object: &corepb.ProcessingResponse{ReadUrl: server.URL, Size: in.Intent.Size, Sha256: in.Intent.ContentHash}}
	provider := ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})
	computer := &Computer{DB: db, Core: core, HTTP: server.Client(), Vision: ai.NewProvider(config.AI{Provider: "dashscope", BaseURL: server.URL, VisionModel: "direct-vision", Dimension: 1024}), Embedding: provider, Summary: provider, SummaryCalls: 16, Profile: in.Profile}
	worker := &DirectWorker{Computer: computer, Publisher: publisher}
	execute := func(kind string, input aiprotocol.Input) uuid.UUID {
		t.Helper()
		completion, _, err := worker.Execute(ctx, directTask(kind, input))
		if err != nil || completion.Outcome != asynccontrol.Succeeded {
			t.Fatal(kind, completion.Outcome, err)
		}
		refs, err := decode[aiprotocol.Published](completion.Refs)
		if err != nil {
			t.Fatal(err)
		}
		return refs.OutputID
	}
	in.SourceID = execute("ai.source.prepare", in)
	in.ContentVersionID = execute("ai.text.build", in)
	in.EmbeddingSetID = execute("ai.embedding.generate", in)
	summaryID := execute("ai.summary.generate", in)
	if visionCalls.Load() != 1 {
		t.Fatal("vision unexpectedly repeated", visionCalls.Load())
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	id, _ := uuid.Parse(file.FileId)
	grant, err := serviceauth.Sign(key, "core-retrieval-v1", time.Now().Add(time.Minute).Unix(), aiprotocol.ScopeGrant{Principal: uuid.New(), Scope: aiprotocol.Scope{Mode: "selected", FileIDs: []uuid.UUID{id}}, Action: "retrieve", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	// Use an actual login with only published-view grants after contraction.
	// Online queries must neither require private tables nor acquire write rights.
	role, password := "direct_query_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
	var schema string
	if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'; GRANT USAGE ON SCHEMA %s TO %s", role, password, schema, role)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec("DROP OWNED BY " + role)
		db.Exec("DROP ROLE " + role)
	})
	var views []string
	if err := db.Raw("SELECT viewname FROM pg_views WHERE schemaname=? AND viewname LIKE 'ai_published_%'", schema).Scan(&views).Error; err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if err := db.Exec("GRANT SELECT ON " + schema + "." + view + " TO " + role).Error; err != nil {
			t.Fatal(err)
		}
	}
	u, err := url.Parse(os.Getenv("TEST_AI_POSTGRES_DSN"))
	if err != nil {
		t.Fatal("invalid test AI DSN")
	}
	u.User = url.UserPassword(role, password)
	params := u.Query()
	params.Set("search_path", schema+",public")
	u.RawQuery = params.Encode()
	queryConfig := config.Config{}
	queryConfig.AISplit.QueryDSN = u.String()
	queryConfig.Postgres.MaxOpenConns = 1
	queryDB, querySQL, err := dependencies.OpenAI(ctx, queryConfig, "ai-query")
	if err != nil {
		t.Fatal("contracted query role failed startup checks", err)
	}
	t.Cleanup(func() { querySQL.Close() })
	for _, sql := range []string{"SELECT * FROM ai_summary_nodes LIMIT 1", "UPDATE ai_objects SET updated_at=updated_at"} {
		if err := queryDB.Exec(sql).Error; err == nil {
			t.Fatal("query role accessed private state", sql)
		}
	}
	query := &Query{DB: queryDB, Core: core, Profile: in.Profile, Provider: provider}
	check := func(content, vectors, summary uuid.UUID) {
		t.Helper()
		overview, err := query.GetOverview(ctx, &pbv2.GetOverviewRequest{FileId: file.FileId, Grant: grant})
		if err != nil || overview.GetSummary() == "" || overview.GetCapabilities().GetText().GetVersionId() != content.String() || overview.GetCapabilities().GetSummary().GetVersionId() != summary.String() {
			t.Fatal("overview lost published version", overview, err)
		}
		if vectors == uuid.Nil {
			if overview.Capabilities.Vector.Available {
				t.Fatal("incompatible vectors remained available")
			}
		} else if overview.Capabilities.Vector.VersionId != vectors.String() {
			t.Fatal("wrong vector pointer", overview.Capabilities.Vector)
		}
		modes := []string{"fulltext", "hybrid"}
		if vectors != uuid.Nil {
			modes = append(modes, "semantic")
		}
		for _, mode := range modes {
			hits, err := query.Search(ctx, &pbv2.SearchRequest{Grant: grant, Query: "BX7319", Mode: mode})
			if err != nil || len(hits.GetHits()) != 1 || hits.Hits[0].Citations[0].ContentVersionId != content.String() {
				t.Fatal("search lost active content", hits, err)
			}
		}
		answer, err := query.Ask(ctx, &pbv2.AskRequest{Grant: grant, Question: "rollback BX7319"})
		if err != nil || len(answer.GetCitations()) == 0 || answer.Citations[0].ContentVersionId != content.String() {
			t.Fatal("ask lost active content", answer, err)
		}
	}
	check(in.ContentVersionID, in.EmbeddingSetID, summaryID)

	// A failed summary rebuild preserves the last usable published result.
	newer := in
	newer.Intent.Seqs.Summary++
	applyDirectIntent(t, publisher, newer)
	completion, err := publisher.ApplyDirect(ctx, directTask("ai.summary.generate", newer), &ComputedValue{OutputID: uuid.New(), FailureCode: "ai.build.rejected"})
	if err != nil || completion.Outcome != asynccontrol.Failed {
		t.Fatal(completion.Outcome, err)
	}
	check(in.ContentVersionID, in.EmbeddingSetID, summaryID)
	newSummary := execute("ai.summary.generate", newer)
	check(in.ContentVersionID, in.EmbeddingSetID, newSummary)

	// Fail at the receipt write, after all normalized rows and pointer updates.
	newer.Intent.Seqs.Text++
	newer.Intent.Seqs.Vector++
	applyDirectIntent(t, publisher, newer)
	task := directTask("ai.text.build", newer)
	value, err := computer.ComputeValue(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE FUNCTION reject_direct_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected receipt failure'; END $$; CREATE TRIGGER reject_direct_receipt BEFORE INSERT ON ai_commit_receipts FOR EACH ROW EXECUTE FUNCTION reject_direct_receipt()`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.ApplyDirect(ctx, task, value); err == nil {
		t.Fatal("receipt failure did not roll back publication")
	}
	if err := db.Exec("DROP TRIGGER reject_direct_receipt ON ai_commit_receipts; DROP FUNCTION reject_direct_receipt()").Error; err != nil {
		t.Fatal(err)
	}
	check(in.ContentVersionID, in.EmbeddingSetID, newSummary)
	if _, err := publisher.ApplyDirect(ctx, task, value); err != nil {
		t.Fatal(err)
	}
	newer.ContentVersionID = value.OutputID
	check(newer.ContentVersionID, uuid.Nil, newSummary)
	newer.EmbeddingSetID = execute("ai.embedding.generate", newer)
	check(newer.ContentVersionID, newer.EmbeddingSetID, newSummary)
	// Late completion from the old input must not replace the new vector set.
	old := directTask("ai.embedding.generate", in)
	oldValue, err := computer.ComputeValue(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if completion, err := publisher.ApplyDirect(ctx, old, oldValue); err != nil || completion.Outcome != asynccontrol.Superseded {
		t.Fatal("obsolete vectors were accepted", completion.Outcome, err)
	}
	check(newer.ContentVersionID, newer.EmbeddingSetID, newSummary)
}
