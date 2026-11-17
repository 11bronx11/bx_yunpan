package aisystem

import (
	"context"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
)

func TestOverviewReportsPublishedIndexVersions(t *testing.T) {
	db, store, publisher, in, file := readyText(t)
	ctx := context.Background()
	core := &scopeClient{file: file}
	query := &Query{DB: db, Core: core, Profile: in.Profile}
	request := &pbv2.GetOverviewRequest{FileId: file.FileId, Grant: "fixture"}
	textOnly, err := query.GetOverview(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if textOnly.ModelVersion != "" || !strings.Contains(textOnly.PipelineVersion, in.Profile.Parser) || !strings.Contains(textOnly.PipelineVersion, in.Profile.Chunker) {
		t.Fatal("text preparation must report its actual pipeline without claiming model output", textOnly)
	}

	provider := ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})
	computer := &Computer{DB: db, Profile: in.Profile, Embedding: provider, Summary: provider, SummaryCalls: 128}
	computePublish(t, publisher, computer, claim(t, store, "ai.embedding.generate", in))
	summaryInput := in
	summaryInput.ContentVersionID = uuid.Nil
	computePublish(t, publisher, computer, claim(t, store, "ai.summary.generate", summaryInput))

	// Configuration changes and a failed rebuild must not relabel existing
	// published results as if the new models had generated them.
	query.Profile.SummaryModel = "future-summary"
	query.Profile.EmbeddingModel = "future-embedding"
	query.Profile.Parser = "future-parser"
	query.Profile.Chunker = "future-chunker"
	if err := db.Exec("UPDATE ai_capability_status SET latest_build_state='failed' WHERE object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	overview, err := query.GetOverview(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if overview.ModelVersion != in.Profile.SummaryModel+"+"+in.Profile.EmbeddingModel {
		t.Fatal("model metadata does not describe published output", overview.ModelVersion)
	}
	wantPipeline := in.Profile.Parser + " · " + in.Profile.Chunker + " · source-map-reduce-v1 · prompt:" + in.Profile.Prompt
	if overview.PipelineVersion != wantPipeline || overview.Summary == "" {
		t.Fatal("published summary or pipeline metadata missing", overview)
	}

	core.file = &corepb.FileRef{FileId: file.FileId, ObjectId: uuid.NewString(), ContentGeneration: 1, MimeType: "text/plain"}
	empty, err := query.GetOverview(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if empty.ModelVersion != "" || empty.PipelineVersion != "" {
		t.Fatal("an unbuilt file must not claim configured model or pipeline versions", empty)
	}
}
