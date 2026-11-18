package aisystem

import (
	"context"
	"errors"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func TestFrozenTasksIgnoreUnrelatedCapabilityChanges(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		change     func(*aiprotocol.Profile)
	}{
		{"text-after-embedding-upgrade", "ai.text.build", func(p *aiprotocol.Profile) { p.EmbeddingModel = "new-model"; p.EmbeddingRevision = "new-revision" }},
		{"text-after-summary-upgrade", "ai.text.build", func(p *aiprotocol.Profile) { p.SummaryModel = "new-model"; p.Prompt = "new-prompt" }},
		{"embedding-after-summary-upgrade", "ai.embedding.generate", func(p *aiprotocol.Profile) { p.SummaryModel = "new-model"; p.SummaryRevision = "new-revision" }},
		{"summary-after-embedding-upgrade", "ai.summary.generate", func(p *aiprotocol.Profile) { p.EmbeddingModel = "new-model"; p.Dimension = 2048 }},
		{"summary-after-parser-upgrade", "ai.summary.generate", func(p *aiprotocol.Profile) { p.Parser = "new-parser"; p.Chunker = "new-chunker" }},
		{"text-projection-after-model-upgrade", "ai.project.text", func(p *aiprotocol.Profile) { p.EmbeddingModel = "new-model"; p.SummaryModel = "new-model" }},
		{"vector-projection-after-model-upgrade", "ai.project.vector", func(p *aiprotocol.Profile) { p.EmbeddingModel = "new-model" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store := databases(t)
			store.PlanContinuations = nil
			_, frozen, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback source"}})
			in := frozen
			current := frozen.Profile
			tc.change(&current)
			provider := &observedProvider{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
			computer := &Computer{DB: db, Profile: current, Embedding: provider, Summary: provider, SummaryCalls: 128,
				Project: func(context.Context, string, aiprotocol.Input) (any, int64, error) {
					return map[string]string{"status": "fixture"}, 1, nil
				},
			}
			task := claim(t, store, tc.kind, in)
			if _, err := computer.Compute(context.Background(), task); err != nil {
				t.Fatal("unrelated capability update rejected frozen work", err)
			}
			var staged int64
			if err := db.Table("ai_artifact_manifests").Where("task_id=? AND kind=?", task.ID, tc.kind).Count(&staged).Error; err != nil || staged != 1 {
				t.Fatal("successful compute did not stage its result", staged, err)
			}
		})
	}
}

func TestFrozenModelTasksRejectRelevantModelChanges(t *testing.T) {
	for _, kind := range []string{"ai.embedding.generate", "ai.summary.generate"} {
		t.Run(kind, func(t *testing.T) {
			db, store := databases(t)
			store.PlanContinuations = nil
			_, in, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback source"}})
			current := in.Profile
			if kind == "ai.embedding.generate" {
				current.EmbeddingModel = "other-geometry"
			} else {
				current.SummaryModel = "other-model"
			}
			provider := &observedProvider{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
			task := claim(t, store, kind, in)
			_, err := (&Computer{DB: db, Profile: current, Embedding: provider, Summary: provider}).Compute(context.Background(), task)
			if !errors.Is(err, asynccontrol.ErrInvalid) || provider.embeddings != 0 || provider.summaries != 0 {
				t.Fatal("live model was called under a different frozen identity", err)
			}
			var staged int64
			if err := db.Table("ai_artifact_manifests").Where("task_id=?", task.ID).Count(&staged).Error; err != nil || staged != 0 {
				t.Fatal("mismatched model staged output", staged, err)
			}
		})
	}
}
