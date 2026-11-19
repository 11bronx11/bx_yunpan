package aisystem

import (
	"context"
	"fmt"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func TestAskSelectedTierEvidenceAndOutputBudgets(t *testing.T) {
	var blocks []rag.Section
	for n := range 80 {
		blocks = append(blocks, rag.Section{Text: fmt.Sprintf("rollback evidence %d", n), HeadingPath: fmt.Sprintf("section %d", n)})
	}
	db, store, _, in, first := readyRetrievalBlocks(t, blocks)
	files := []*corepb.FileRef{first}
	for n := 1; n < 10; n++ {
		_, _, file := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: fmt.Sprintf("rollback other file %d", n)}})
		files = append(files, file)
	}
	in.Profile.EnableVectors = false
	for _, tc := range []struct {
		mode                           string
		files, evidence, output, scale int
	}{
		{"all", 1, 8, 0, 0},
		{"selected", 1, 40, 20480, 5},
		{"selected", 3, 32, 16384, 4},
		{"selected", 5, 24, 12288, 3},
		{"selected", 9, 16, 8192, 2},
		{"selected", 10, 8, 4096, 1},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.mode, tc.files), func(t *testing.T) {
			probe := &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
			q := &Query{DB: db, Core: &parityCore{files: files[:tc.files]}, Provider: probe, Profile: in.Profile, ScopeLimit: 100, RerankCandidates: 30, RerankTopK: 8}
			out, err := q.Ask(context.Background(), &pbv2.AskRequest{Grant: retrievalGrant(t, tc.mode, files[:tc.files]...), Question: "rollback"})
			if err != nil {
				t.Fatal(err)
			}
			if len(probe.evidence) != tc.evidence || len(out.Citations) != tc.evidence || probe.options.MaxTokens != tc.output || probe.options.TimeoutMultiplier != tc.scale {
				t.Fatalf("evidence=%d citations=%d options=%+v, want evidence=%d output=%d scale=%d", len(probe.evidence), len(out.Citations), probe.options, tc.evidence, tc.output, tc.scale)
			}
			for _, c := range out.Citations {
				found := false
				for _, f := range files[:tc.files] {
					found = found || c.FileId == f.FileId
				}
				if !found {
					t.Fatal("evidence escaped selected scope", c.FileId)
				}
			}
			if tc.files < 10 && len(out.Coverage.MissingEvidenceFileIds) != 0 || tc.files == 10 && len(out.Coverage.MissingEvidenceFileIds) != 2 {
				t.Fatal("incorrect selected-file coverage", out.Coverage)
			}
		})
	}
}
