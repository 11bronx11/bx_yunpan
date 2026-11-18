package aisystem

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type checkpointProbe struct {
	ai.Provider
	calls, failAt int
}

func TestSummaryCheckpointGCRequiresTerminalControlProof(t *testing.T) {
	db, store := databases(t)
	store.PlanContinuations = nil
	publisher, in, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "summary source"}})
	task := claim(t, store, "ai.summary.generate", in)
	c := &Computer{DB: db, Summary: &checkpointProbe{}, SummaryCalls: 30}
	if _, _, err := c.summarizeTask(context.Background(), []string{"source"}, task, in); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE ai_artifact_manifests SET created_at=? WHERE task_id=?", time.Now().Add(-10*24*time.Hour), task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if ids, _, err := publisher.collectableArtifacts(context.Background(), in.Intent); err != nil || len(ids) != 0 {
		t.Fatal("GC collected a live computation's checkpoint", ids, err)
	}
	if err := store.DB.Model(&asynccontrol.Task{}).Where("id=?", task.ID).Update("state", asynccontrol.Succeeded).Error; err != nil {
		t.Fatal(err)
	}
	ids, _, err := publisher.collectableArtifacts(context.Background(), in.Intent)
	if err != nil || len(ids) != 1 {
		t.Fatal("terminal checkpoint did not enter bounded artifact GC", ids, err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := publisher.cleanArtifacts(tx, in.Intent, ids, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("ai_summary_checkpoints").Where("task_id=?", task.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("checkpoint was not removed", count, err)
	}
}

func (p *checkpointProbe) Enrich(_ context.Context, text string) (ai.Insight, error) {
	p.calls++
	if p.calls == p.failAt {
		return ai.Insight{}, errors.New("transient fixture")
	}
	summary := "part " + aiprotocol.Hash([]byte(text))[:12]
	if strings.Contains(text, "TAIL937") {
		summary += " TAIL937"
	}
	return ai.Insight{Summary: summary}, nil
}

func TestSummaryRetryResumesImmutablePartsAcrossGenerations(t *testing.T) {
	db, store := databases(t)
	store.PlanContinuations = nil
	blocks := []rag.Section{{Text: "HEAD " + strings.Repeat("甲", 5500)}, {Text: "MIDDLE " + strings.Repeat("乙", 5500)}, {Text: strings.Repeat("丙", 5500) + " TAIL937"}}
	publisher, in, _ := appendRetrievalBlocks(t, db, store, blocks)
	task := claim(t, store, "ai.summary.generate", in)
	probe := &checkpointProbe{failAt: 3}
	computer := &Computer{DB: db, Profile: in.Profile, Summary: probe, SummaryCalls: 30}
	_, err := computer.Compute(context.Background(), task)
	if err == nil || IsPermanentComputeError(err) {
		t.Fatal("fixture must stop before the last part", err)
	}
	var nodes, published int64
	if e := db.Table("ai_summary_checkpoints").Where("task_id=?", task.ID).Count(&nodes).Error; e != nil || nodes != 2 {
		t.Fatal("completed parts were not persisted", nodes, e)
	}
	if e := db.Table("ai_published_summaries").Count(&published).Error; e != nil || published != 0 {
		t.Fatal("partial summary became visible", published, e)
	}
	if e := store.Fail(context.Background(), task, err, false); e != nil {
		t.Fatal(e)
	}
	if e := store.DB.Exec("UPDATE tasks SET not_before=clock_timestamp() WHERE id=?", task.ID).Error; e != nil {
		t.Fatal(e)
	}
	retry := claim(t, store, "ai.summary.generate", in)
	if retry.Generation != task.Generation+1 {
		t.Fatal("fixture did not use the next control generation")
	}
	// Recreate the computer/provider to prove recovery uses PG, not process memory.
	resumed := &checkpointProbe{}
	computePublish(t, publisher, &Computer{DB: db, Profile: in.Profile, Summary: resumed, SummaryCalls: 30}, retry)
	wantCalls := len(rag.SummaryParts(blocks, 6000)) - 2 + 1
	if resumed.calls != wantCalls {
		t.Fatal("retry repeated finished map nodes", resumed.calls, wantCalls)
	}
	var summary string
	if e := db.Table("ai_published_summaries").Select("summary").Scan(&summary).Error; e != nil || !strings.Contains(summary, "TAIL937") {
		t.Fatal("resumed summary lost the tail", summary, e)
	}
}

func TestSummaryCheckpointsRejectTamperingAndIsolateNewInputs(t *testing.T) {
	db, store := databases(t)
	store.PlanContinuations = nil
	_, in, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "summary source"}})
	task := claim(t, store, "ai.summary.generate", in)
	probe := &checkpointProbe{}
	c := &Computer{DB: db, Summary: probe, SummaryCalls: 30}
	parts := []string{"source"}
	if _, _, err := c.summarizeTask(context.Background(), parts, task, in); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.summarizeTask(context.Background(), parts, task, in); err != nil || probe.calls != 1 {
		t.Fatal("finished reduce node was not reused", err, probe.calls)
	}
	changed := in
	changed.Intent.RequestID = uuid.New()
	changed.Profile.SummaryRevision = "next"
	next := claim(t, store, "ai.summary.generate", changed)
	if _, _, err := c.summarizeTask(context.Background(), parts, next, changed); err != nil || probe.calls != 2 {
		t.Fatal("new model/source identity reused another task checkpoint", err, probe.calls)
	}
	if err := db.Exec("UPDATE ai_artifact_manifests SET data='{}' WHERE task_id=? AND kind LIKE 'ai.summary.checkpoint.v1:%'", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.summarizeTask(context.Background(), parts, task, in); !errors.Is(err, asynccontrol.ErrInvalid) || probe.calls != 2 {
		t.Fatal("corrupt checkpoint was used or silently recomputed", err, probe.calls)
	}
}
