package aisystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func databases(t *testing.T) (*gorm.DB, *asynccontrol.Store) {
	t.Helper()
	aiDSN, ctrlDSN := os.Getenv("TEST_AI_POSTGRES_DSN"), os.Getenv("TEST_CONTROL_POSTGRES_DSN")
	if aiDSN == "" || ctrlDSN == "" {
		t.Skip("dedicated AI and control databases required")
	}
	if aiDSN == ctrlDSN {
		t.Fatal("AI/control databases must differ")
	}
	open := func(dsn, dir string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal("database connection failed")
		}
		sql, _ := db.DB()
		sql.SetMaxOpenConns(1)
		schema := "ai_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("SET search_path TO " + schema + ", public").Error; err != nil {
			t.Fatal(err)
		}
		paths, _ := filepath.Glob("../../" + dir + "/*.up.sql")
		sort.Strings(paths)
		for _, p := range paths {
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			if err := db.Exec(string(b)).Error; err != nil {
				t.Fatalf("migration %s: %v", p, err)
			}
		}
		t.Cleanup(func() { _ = db.Exec("DROP SCHEMA " + schema + " CASCADE").Error; _ = sql.Close() })
		return db
	}
	db := open(aiDSN, "ai-migrations")
	store := asynccontrol.New(open(ctrlDSN, "control-migrations"))
	Register(store.Registry)
	p := &Planner{}
	store.PlanContinuations = p.Continuations
	store.PlanFailure = p.Failure
	store.Observe = p.Observe
	store.AfterTerminal = p.Finish
	return db, store
}
func testInput() aiprotocol.Input {
	return aiprotocol.Input{TargetEpoch: 1, Intent: aiprotocol.Intent{Version: 2, RequestedAt: time.Now().Unix(), RequestID: uuid.New(), ObjectID: uuid.New(), ContentGeneration: 1, ContentHash: strings.Repeat("a", 64), Size: 10, MediaType: "text/plain", ProfileID: "default", ProfileRevision: "v1", Seqs: aiprotocol.Seqs{Source: 1, Text: 1, Vector: 1, Summary: 1}, Targets: []string{"source", "text", "vectors", "summary"}}, Profile: aiprotocol.Profile{ID: "default", Revision: "v1", Parser: "parser-v1", Chunker: rag.ParentChildVersion, EmbeddingProvider: "fake", EmbeddingModel: "fake", EmbeddingRevision: "v1", SummaryProvider: "fake", SummaryModel: "fake", SummaryRevision: "v1", Prompt: "v1", Dimension: 1024, EnableVectors: true, EnableSummary: true}}
}
func claim(t *testing.T, s *asynccontrol.Store, kind string, in aiprotocol.Input) asynccontrol.Task {
	t.Helper()
	ctx := context.Background()
	sp := spec(kind, in)
	task, err := s.Ensure(ctx, sp, "test")
	if err != nil {
		t.Fatal(err)
	}
	reserved, ok, err := s.Reserve(ctx, task.Queue)
	if err != nil || !ok {
		t.Fatal("reserve", ok, err)
	}
	if reserved.ID != task.ID {
		t.Fatal("unexpected queued task", reserved.Kind)
	}
	task, ok, err = s.Claim(ctx, reserved.Notice())
	if err != nil || !ok {
		t.Fatal("claim", ok, err)
	}
	return task
}
func stage(t *testing.T, db *gorm.DB, s *asynccontrol.Store, task asynccontrol.Task, in aiprotocol.Input, data any, id uuid.UUID, rows int64) (asynccontrol.Result, aiprotocol.Output) {
	t.Helper()
	b, _ := json.Marshal(data)
	hash, b, err := asynccontrol.Digest(b)
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{ObjectID: in.Intent.ObjectID, ContentGeneration: in.Intent.ContentGeneration, ArtifactID: uuid.New(), TaskID: task.ID, Generation: task.Generation, Kind: task.Kind, InputDigest: task.InputDigest, ArtifactHash: hash, TypedOutputRef: id, RowCount: rows, ByteCount: int64(len(b)), Complete: true, Data: b}
	if err := db.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	out := aiprotocol.Output{Version: 2, TaskID: task.ID, Generation: task.Generation, InputDigest: task.InputDigest, ArtifactID: a.ArtifactID, ArtifactHash: hash, OutputID: id, ObjectID: in.Intent.ObjectID, ContentGeneration: 1, RowCount: rows, ByteCount: int64(len(b)), Coverage: "complete", Complete: true}
	body, _ := json.Marshal(out)
	r, err := s.SaveResult(context.Background(), task, body)
	if err != nil {
		t.Fatal(err)
	}
	return r, out
}
func publish(t *testing.T, p *Publisher, task asynccontrol.Task, r asynccontrol.Result) {
	t.Helper()
	outcome, err := p.Publish(context.Background(), task, r)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != asynccontrol.Succeeded {
		t.Fatal(outcome)
	}
	if err := p.Control.Report(context.Background(), task, r, outcome); err != nil {
		t.Fatal(err)
	}
}
func seedObject(t *testing.T, db *gorm.DB, in aiprotocol.Input) {
	t.Helper()
	i := in.Intent
	if err := db.Exec(`INSERT INTO ai_objects(object_id,content_generation,content_hash,size,media_type,profile_id,profile_revision,desired_source_seq,desired_text_seq,desired_vector_seq,desired_summary_seq) VALUES(?,?,?,?,?,?,?,1,1,1,1)`, i.ObjectID, 1, i.ContentHash, i.Size, i.MediaType, i.ProfileID, i.ProfileRevision).Error; err != nil {
		t.Fatal(err)
	}
}
func TestAcceptedPublishReceiptAndAtomicContinuations(t *testing.T) {
	db, s := databases(t)
	ctx := context.Background()
	in := testInput()
	seedObject(t, db, in)
	p := &Publisher{DB: db, Control: s}
	task := claim(t, s, "ai.source.prepare", in)
	source := uuid.New()
	r, _ := stage(t, db, s, task, in, sourceData{Parsed: ai.ParseResult{ParserID: "text", ParserVersion: "v1", Sections: []rag.Section{{Text: "发布协议 alpha 尾部回滚保留", Kind: "text"}}}, Coverage: "complete"}, source, 1)
	var visible int64
	_ = db.Table("ai_published_sources").Count(&visible)
	if visible != 0 {
		t.Fatal("staging visible")
	}
	var state string
	_ = s.DB.Table("tasks").Select("state").Where("id=?", task.ID).Scan(&state)
	if state != asynccontrol.Computed {
		t.Fatal("AI effect prematurely succeeded", state)
	}
	var children int64
	_ = s.DB.Table("tasks").Where("parent_id=?", task.ID).Count(&children)
	if children != 0 {
		t.Fatal("children before receipt")
	}
	// Crash window: AI transaction committed; control report was lost.
	outcome, err := p.Publish(ctx, task, r)
	if err != nil || outcome != asynccontrol.Succeeded {
		t.Fatal(outcome, err)
	}
	again, err := p.Publish(ctx, task, r)
	if err != nil || again != outcome {
		t.Fatal("receipt replay", err)
	}
	if err := s.Report(ctx, task, r, outcome); err != nil {
		t.Fatal(err)
	}
	if err := s.Report(ctx, task, r, outcome); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.Table("tasks").Where("parent_id=?", task.ID).Count(&children)
	if children != 2 {
		t.Fatal("lost/duplicate continuations", children)
	}
	var receipts int64
	_ = db.Table("ai_commit_receipts").Count(&receipts)
	if receipts != 1 {
		t.Fatal("duplicate receipt")
	}
	// Continue text independently from embedding/summary and prove PG FTS reads it.
	in.SourceID = source
	textTask := claim(t, s, "ai.text.build", in)
	version := uuid.New()
	chunks, err := rag.BuildParentChild(version, []rag.Section{{Text: "发布协议 alpha 尾部回滚保留", Kind: "text"}})
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := stage(t, db, s, textTask, in, textData{chunks}, version, int64(len(chunks.Parents)+len(chunks.Children)))
	publish(t, p, textTask, tr)
	q := &Query{DB: db}
	hits, err := q.pgText(ctx, []string{version.String()}, "回滚", 10)
	if err != nil || len(hits) != 1 {
		t.Fatal("text depends on vector/summary", len(hits), err)
	}
	var obj Object
	if err := db.First(&obj, "object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	if obj.ActiveContentVersionID == nil || obj.ActiveEmbeddingSetID != nil || obj.ActiveSummaryID != nil {
		t.Fatal("capability independence")
	}
}
func TestSupersededStagingAndTombstoneNeverBecomeVisible(t *testing.T) {
	db, s := databases(t)
	ctx := context.Background()
	in := testInput()
	seedObject(t, db, in)
	p := &Publisher{DB: db, Control: s}
	task := claim(t, s, "ai.source.prepare", in)
	// Expired control lease rejects the artifact before publication.
	if err := s.DB.Exec("UPDATE tasks SET lease_until=clock_timestamp()-interval '1 second' WHERE id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResult(ctx, task, []byte(`{"complete":true}`)); err != asynccontrol.ErrLost {
		t.Fatal("stale worker accepted", err)
	}
	// Separate higher intent makes an already accepted old output superseded.
	in2 := testInput()
	seedObject(t, db, in2)
	second := claim(t, s, "ai.source.prepare", in2)
	r, _ := stage(t, db, s, second, in2, sourceData{Parsed: ai.ParseResult{Sections: []rag.Section{{Text: "old", Kind: "text"}}}}, uuid.New(), 1)
	_ = db.Exec("UPDATE ai_objects SET desired_source_seq=2 WHERE object_id=?", in2.Intent.ObjectID)
	outcome, err := p.Publish(ctx, second, r)
	if err != nil || outcome != asynccontrol.Superseded {
		t.Fatal(outcome, err)
	}
	if err := s.Report(ctx, second, r, outcome); err != nil {
		t.Fatal(err)
	}
	var released int64
	_ = s.DB.Table("task_continuations").Where("accepted_result_id=? AND state='released'", r.ID).Count(&released)
	if released != 0 {
		t.Fatal("superseded released children")
	}
	// Same generation tombstones are permanent, even if a later intent arrives.
	_ = db.Exec("UPDATE ai_objects SET tombstoned_at=clock_timestamp() WHERE object_id=?", in2.Intent.ObjectID)
	in2.Intent.RequestID = uuid.New()
	in2.Intent.Seqs.Source = 3
	intent := claim(t, s, "ai.intent.apply", in2)
	ir, _ := stage(t, db, s, intent, in2, in2.Intent, uuid.New(), 1)
	outcome, err = p.Publish(ctx, intent, ir)
	if err != nil || outcome != asynccontrol.Superseded {
		t.Fatal("tombstone resurrected", outcome, err)
	}
}
