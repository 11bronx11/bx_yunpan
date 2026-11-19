package aisystem

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func directDatabases(t *testing.T) (*gorm.DB, *asynccontrol.Store) {
	t.Helper()
	db, s := databases(t)
	s.ProtocolVersion = 3
	if e := s.DB.Exec("SET bx.coordination_protocol='v3';UPDATE control_settings SET value='v3' WHERE key='coordination_protocol'").Error; e != nil {
		t.Fatal(e)
	}
	if e := db.Exec("SET bx.ai_protocol='v3'").Error; e != nil {
		t.Fatal(e)
	}
	s.PlanCompleted = (&Planner{}).Completed
	return db, s
}

func TestDirectTransfersLegacySummaryNodeWithoutModelReplay(t *testing.T) {
	db, s := databases(t)
	s.PlanContinuations = nil
	ctx := context.Background()
	_, in, _ := appendRetrievalBlocks(t, db, s, []rag.Section{{Text: "legacy summary source"}})
	task := claim(t, s, "ai.summary.generate", in)
	probe := &checkpointProbe{}
	c := &Computer{DB: db, Profile: in.Profile, Summary: probe, SummaryCalls: 30}
	if _, _, e := c.summarizeTask(ctx, []string{"legacy summary source"}, task, in); e != nil {
		t.Fatal(e)
	}
	if e := s.DB.Model(&asynccontrol.Task{}).Where("id=?", task.ID).Update("lease_until", gorm.Expr("clock_timestamp()-interval '1 second'")).Error; e != nil {
		t.Fatal(e)
	}
	s.ProtocolVersion = 3
	if e := s.DB.Exec("SET bx.coordination_protocol='v3'").Error; e != nil {
		t.Fatal(e)
	}
	if _, e := s.HandoverDirectBatch(ctx, 100); e != nil {
		t.Fatal(e)
	}
	var current asynccontrol.Task
	if e := s.DB.First(&current, "id=?", task.ID).Error; e != nil || current.State != asynccontrol.Pending {
		t.Fatal("expired legacy runner was not made claimable", current.State, e)
	}
	m := &DirectMigrator{DB: db, Control: s}
	if copied, retained, e := m.TransferNodes(ctx); e != nil || copied != 1 || retained != 0 {
		t.Fatal(copied, retained, e)
	}
	if _, _, e := c.summarizeTask(ctx, []string{"legacy summary source"}, current, in); e != nil || probe.calls != 1 {
		t.Fatal("migrated node repeated the model call", probe.calls, e)
	}
	var unknown Artifact
	if e := db.Where("task_id=?", task.ID).Take(&unknown).Error; e != nil {
		t.Fatal(e)
	}
	unknown.ArtifactID, unknown.Kind = uuid.New(), "ai.project.text"
	if e := db.Create(&unknown).Error; e != nil {
		t.Fatal(e)
	}
	contract, e := os.ReadFile("../../ai-contract-migrations/000001_archive_artifacts.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Archive(ctx, string(contract)); e == nil {
		t.Fatal("unreconciled legacy artifact was archived")
	}
}

func TestDirectAIContractAndSummaryNodeReuse(t *testing.T) {
	db, s := directDatabases(t)
	ctx := context.Background()
	p := &Publisher{DB: db, Control: s}
	in := testInput()
	applyDirectIntent(t, p, in)
	contract, e := os.ReadFile("../../ai-contract-migrations/000001_archive_artifacts.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	m := &DirectMigrator{DB: db, Control: s}
	if e = m.Archive(ctx, string(contract)); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Exec("DROP SCHEMA ai_v2_archive CASCADE") })
	if _, _, e = m.TransferNodes(ctx); e != nil {
		t.Fatal("contracted restart summary transfer", e)
	}
	var count int64
	if e = db.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE'").Scan(&count).Error; e != nil || count != 15 {
		t.Fatal("contracted table count", count, e)
	}
	task := directTask("ai.source.prepare", in)
	in.SourceID = uuid.New()
	if _, e = p.ApplyDirect(ctx, task, &ComputedValue{OutputID: in.SourceID, Value: sourceData{Parsed: ai.ParseResult{Sections: []ai.Section{{Text: "after contract", Kind: "paragraph"}}}, Coverage: "complete"}, RowCount: 1, Coverage: "complete"}); e != nil {
		t.Fatal(e)
	}
	probe := &checkpointProbe{}
	c := &Computer{DB: db, Profile: in.Profile, Summary: probe, SummaryCalls: 30}
	task = directTask("ai.summary.generate", in)
	for n := 0; n < 2; n++ {
		task.Generation++
		if _, _, e = c.summarizeTask(ctx, []string{"after contract"}, task, in); e != nil {
			t.Fatal(e)
		}
	}
	if probe.calls != 1 {
		t.Fatal("summary retry repeated provider call", probe.calls)
	}
	if _, e = p.ApplyDirect(ctx, directTask("ai.artifact.gc", in), &ComputedValue{OutputID: uuid.New(), RowCount: 1}); e != nil {
		t.Fatal("GC still depends on artifacts", e)
	}
}

func directTask(kind string, in aiprotocol.Input) asynccontrol.Task {
	raw := jsonValue(in)
	digest, _, _ := asynccontrol.Digest(raw)
	return asynccontrol.Task{ID: uuid.New(), Kind: kind, Input: raw, InputDigest: digest, ProtocolVersion: 3, ContractVersion: 1, Phase: "execute", Generation: 1, EffectTarget: asynccontrol.EffectAI, Deadline: time.Now().Add(time.Hour), ResourceKey: "object:" + in.Intent.ObjectID.String() + ":1"}
}

func applyDirectIntent(t *testing.T, p *Publisher, in aiprotocol.Input) {
	t.Helper()
	task := directTask("ai.intent.apply", in)
	if _, e := p.ApplyDirect(context.Background(), task, &ComputedValue{OutputID: uuid.New(), Value: in.Intent, RowCount: 1, Coverage: "complete"}); e != nil {
		t.Fatal(e)
	}
}

func TestDirectAIWinnerReceiptAndAtomicRollback(t *testing.T) {
	db, s := directDatabases(t)
	ctx := context.Background()
	p := &Publisher{DB: db, Control: s}
	in := testInput()
	applyDirectIntent(t, p, in)
	task := directTask("ai.source.prepare", in)
	id := uuid.New()
	value := &ComputedValue{OutputID: id, Value: sourceData{Parsed: ai.ParseResult{Sections: []ai.Section{{Text: "first publication", Kind: "paragraph"}}}, Coverage: "complete"}, RowCount: 1, Coverage: "complete"}
	if e := db.Exec(`CREATE FUNCTION reject_source_block() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected normalized write failure'; END $$; CREATE TRIGGER reject_source_block BEFORE INSERT ON ai_source_blocks FOR EACH ROW EXECUTE FUNCTION reject_source_block()`).Error; e != nil {
		t.Fatal(e)
	}
	if _, e := p.ApplyDirect(ctx, task, value); e == nil {
		t.Fatal("partial normalized write committed")
	}
	var count int64
	db.Table("ai_sources").Count(&count)
	if count != 0 {
		t.Fatal("source escaped rollback")
	}
	var object Object
	db.First(&object, "object_id=? AND content_generation=1", in.Intent.ObjectID)
	if object.ActiveSourceID != nil {
		t.Fatal("pointer escaped rollback")
	}
	db.Exec("DROP TRIGGER reject_source_block ON ai_source_blocks;DROP FUNCTION reject_source_block()")
	first, e := p.ApplyDirect(ctx, task, value)
	if e != nil {
		t.Fatal(e)
	}
	value.Value = sourceData{Parsed: ai.ParseResult{Sections: []ai.Section{{Text: "different retry", Kind: "paragraph"}}}, Coverage: "complete"}
	value.OutputID = uuid.New()
	task.Generation++
	second, e := p.ApplyDirect(ctx, task, value)
	if e != nil {
		t.Fatal(e)
	}
	if string(first.Refs) != string(second.Refs) {
		t.Fatal("retry replaced domain winner")
	}
	found, ok, e := p.Lookup(ctx, task)
	if e != nil || !ok || string(found.Refs) != string(first.Refs) {
		t.Fatal("receipt recovery", ok, e)
	}
	originalDigest := task.InputDigest
	task.InputDigest = "different"
	if _, _, e = p.Lookup(ctx, task); !errors.Is(e, asynccontrol.ErrConflict) {
		t.Fatal("different input accepted", e)
	}
	db.Table("ai_artifact_manifests").Count(&count)
	if count != 0 {
		t.Fatal("direct path wrote generic artifact")
	}
	s.DB.Table("results").Count(&count)
	if count != 0 {
		t.Fatal("direct path wrote control result")
	}
	task.InputDigest = originalDigest
	var legacy Receipt
	if e = db.Where("task_id=?", task.ID).Take(&legacy).Error; e != nil {
		t.Fatal(e)
	}
	legacy.ResultID, legacy.ProtocolVersion = uuid.New(), 2
	if e = db.Create(&legacy).Error; e != nil {
		t.Fatal(e)
	}
	if _, _, e = p.Lookup(ctx, task); !errors.Is(e, asynccontrol.ErrConflict) {
		t.Fatal("ambiguous historical receipts accepted", e)
	}
}

func TestDirectAISupersededAndTombstone(t *testing.T) {
	db, s := directDatabases(t)
	ctx := context.Background()
	p := &Publisher{DB: db, Control: s}
	in := testInput()
	applyDirectIntent(t, p, in)
	stale := directTask("ai.source.prepare", in)
	newer := in
	newer.Intent.Seqs.Source++
	applyDirectIntent(t, p, newer)
	v := &ComputedValue{OutputID: uuid.New(), Value: sourceData{Parsed: ai.ParseResult{Sections: []ai.Section{{Text: "obsolete", Kind: "paragraph"}}}, Coverage: "complete"}, RowCount: 1, Coverage: "complete"}
	c, e := p.ApplyDirect(ctx, stale, v)
	if e != nil || c.Outcome != asynccontrol.Superseded {
		t.Fatal(c.Outcome, e)
	}
	tomb := directTask("ai.object.tombstone", newer)
	if _, e = p.ApplyDirect(ctx, tomb, &ComputedValue{OutputID: uuid.New(), RowCount: 1}); e != nil {
		t.Fatal(e)
	}
	c, e = p.ApplyDirect(ctx, directTask("ai.source.prepare", newer), v)
	if e != nil || c.Outcome != asynccontrol.Superseded {
		t.Fatal("tombstone resurrected", c.Outcome, e)
	}
	if e = db.Exec("SET bx.ai_protocol='v2'").Error; e != nil {
		t.Fatal(e)
	}
	if e = db.Model(&Object{}).Where("object_id=?", in.Intent.ObjectID).Update("desired_source_seq", 100).Error; e == nil {
		t.Fatal("legacy AI writer crossed fence")
	}
}

func TestDirectAICompletionRegistersChildrenAndJobRefs(t *testing.T) {
	db, s := directDatabases(t)
	ctx := context.Background()
	in := testInput()
	in.Intent.JobID = uuid.New()
	in.Intent.OwnerID = uuid.New()
	in.Profile.EnableSummary = false
	in.Profile.EnableVectors = false
	in.Intent.Targets = []string{"source", "text"}
	in.ProjectionBackends = []string{"postgres"}
	task := claim(t, s, "ai.intent.apply", in)
	p := &Publisher{DB: db, Control: s}
	v, e := (&Computer{DB: db, Profile: in.Profile}).ComputeValue(ctx, task)
	if e != nil {
		t.Fatal(e)
	}
	c, e := p.ApplyDirect(ctx, task, v)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Complete(ctx, task, c); e != nil {
		t.Fatal(e)
	}
	var source asynccontrol.Task
	if e = s.DB.Where("kind='ai.source.prepare'").Take(&source).Error; e != nil {
		t.Fatal(e)
	}
	reserved, ok, e := s.Reserve(ctx, source.Queue)
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	source, ok, e = s.Claim(ctx, reserved.Notice())
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	c, e = p.ApplyDirect(ctx, source, &ComputedValue{OutputID: uuid.New(), Value: sourceData{Parsed: ai.ParseResult{Sections: []ai.Section{{Text: "searchable text", Kind: "paragraph"}}}, Coverage: "complete"}, RowCount: 1, Coverage: "complete"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Complete(ctx, source, c); e != nil {
		t.Fatal(e)
	}
	var link struct{ CompletionRefs string }
	if e = s.DB.Table("ai_job_task_links").Select("completion_refs::text AS completion_refs").Where("task_id=?", source.ID).Take(&link).Error; e != nil {
		t.Fatal(e)
	}
	if link.CompletionRefs == "{}" {
		t.Fatal("missing job version references")
	}
	var agg struct{ FinishTaskID *uuid.UUID }
	s.DB.Table("ai_job_aggregates").Where("job_id=?", in.Intent.JobID).Take(&agg)
	if agg.FinishTaskID != nil {
		t.Fatal("job finished before text successor")
	}
}

func TestDirectAIUnknownReceiptClosesJobAsNeedsAttention(t *testing.T) {
	db, s := directDatabases(t)
	ctx := context.Background()
	in := testInput()
	in.Intent.JobID, in.Intent.OwnerID = uuid.New(), uuid.New()
	in.Intent.Targets = []string{"source", "text"}
	in.Profile.EnableSummary, in.Profile.EnableVectors = false, false
	in.ProjectionBackends = []string{"postgres"}
	task := claim(t, s, "ai.intent.apply", in)
	p := &Publisher{DB: db, Control: s}
	c, e := p.ApplyDirect(ctx, task, &ComputedValue{OutputID: uuid.New(), RowCount: 1})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Complete(ctx, task, c); e != nil {
		t.Fatal(e)
	}
	r, ok, e := s.Reserve(ctx, "ai-content")
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	task, ok, e = s.Claim(ctx, r.Notice())
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	if e = s.Fail(ctx, task, errors.New("permanent fixture error"), true); e != nil {
		t.Fatal(e)
	}
	r, ok, e = s.Reserve(ctx, "ai-content")
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	task, ok, e = s.Claim(ctx, r.Notice())
	if e != nil || !ok || task.Phase != "reconcile" {
		t.Fatal("final receipt check", task.Phase, ok, e)
	}
	if e = s.Fail(ctx, task, asynccontrol.ErrReceiptMissing, true); e != nil {
		t.Fatal(e)
	}
	var finish asynccontrol.Task
	if e = s.DB.Where("kind='ai.job.finish'").Take(&finish).Error; e != nil {
		t.Fatal("job left waiting after final receipt check", e)
	}
	conclusion, e := decode[aiprotocol.JobFinish](finish.Input)
	if e != nil || conclusion.Status != asynccontrol.NeedsAttention || conclusion.Outcomes["text"] != asynccontrol.NeedsAttention {
		t.Fatal("unknown commit was reported as failure", conclusion, e)
	}
}
