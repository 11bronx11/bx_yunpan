package aisystem

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestFrozenChunkerCompatibilityAndEmbeddingContext(t *testing.T) {
	for _, tc := range []struct{ version, text string }{
		{"parent-child-v1", "body"},
		{"parent-child-v2", "body"},
		{"parent-child-v3", "heading\n\nbody"},
		{rag.ParentChildVersion, "heading\n\nbody"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			current := aiprotocol.Profile{Chunker: rag.ParentChildVersion}
			frozen := current
			frozen.Chunker = tc.version
			if !computeProfileCompatible("ai.text.build", frozen, current, "text/plain") || embeddingText(tc.version, "heading", "body") != tc.text {
				t.Fatal("upgrade changed accepted chunker or embedding input", tc.version)
			}
		})
	}
}

func TestChunkBoundaryPublishedSearchAndAsk(t *testing.T) {
	runes := make([]rune, 2000)
	for i := range runes {
		runes[i] = rune(0x4e00 + i)
	}
	for _, tc := range []struct {
		name, query string
		block       rag.Section
	}{
		{"text", "预算", rag.Section{Text: strings.Repeat("甲", 1599) + "预算已经批准" + strings.Repeat("乙", 1700), Kind: rag.KindText}},
		{"table", string(runes[590:592]), rag.Section{Text: "| 名称 |\n| --- |\n| " + string(runes) + " |", Kind: rag.KindTable, HasHeader: true, SheetName: "预算", RowStart: 2, RowEnd: 2, ColumnStart: 1, ColumnEnd: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, _, in, file := readyRetrievalBlocks(t, []rag.Section{tc.block})
			for _, backend := range []string{"postgres", "elasticsearch"} {
				t.Run(backend, func(t *testing.T) {
					probe := &retrievalProbe{Provider: ai.NewProvider(config.AI{Dimension: 1024})}
					q := &Query{DB: db, Core: &scopeClient{file: file}, Profile: in.Profile, Provider: probe, RerankCandidates: 30, RerankTopK: 8}
					if backend == "elasticsearch" {
						endpoint := os.Getenv("TEST_SEARCH_ES_ENDPOINT")
						if endpoint == "" {
							t.Skip("dedicated Elasticsearch endpoint required")
						}
						in.TargetNamespace = "bx-chunk-" + strings.ReplaceAll(uuid.NewString(), "-", "")
						projector := &Projector{DB: db, ES: endpoint, HTTP: &http.Client{Timeout: 15 * time.Second}}
						_, target, _, err := projectionIdentity("es", in)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := projector.request(context.Background(), "DELETE", strings.TrimRight(endpoint, "/")+"/"+target, nil, "es", nil); err != nil {
								t.Error("owned boundary index cleanup failed", err)
							}
						})
						data, count, err := projector.Project(context.Background(), "ai.project.text", in)
						if err != nil {
							t.Fatal(err)
						}
						if err := db.Transaction(func(tx *gorm.DB) error {
							return publishProjection(tx, in, Artifact{Kind: "ai.project.text", Data: jsonValue(data), RowCount: count})
						}); err != nil {
							t.Fatal(err)
						}
						q.Projector = projector
					}
					out, err := q.Search(context.Background(), &pbv2.SearchRequest{Query: tc.query, Mode: "fulltext"})
					if err != nil || len(out.GetHits()) != 1 || out.Retrieval.TextBackend != backend || len(out.Hits[0].Citations) == 0 {
						t.Fatalf("boundary term not recalled by %s: %v %v", backend, out, err)
					}
					for _, mode := range []string{"all", "selected"} {
						answer, err := q.Ask(context.Background(), &pbv2.AskRequest{Grant: retrievalGrant(t, mode, file), Question: tc.query})
						if err != nil || len(answer.GetCitations()) == 0 || answer.Retrieval.TextBackend != backend || len(probe.evidence) == 0 {
							t.Fatalf("boundary term lost before %s Ask evidence: %v %v", mode, answer, err)
						}
						found := false
						for _, evidence := range probe.evidence {
							found = found || strings.Contains(evidence.Content, tc.query)
						}
						if !found {
							t.Fatal("recalled boundary term absent from parent evidence")
						}
						for _, cite := range answer.Citations {
							if cite.ContentVersionId != in.ContentVersionID.String() || cite.SourceVersionId != in.SourceID.String() || cite.FileId != file.FileId {
								t.Fatal("boundary citation lost its published version", cite)
							}
							if tc.name == "table" && (cite.SourceLocation == nil || cite.SourceLocation.SheetName != "预算" || cite.SourceLocation.RowStart != 2 || cite.SourceLocation.RowEnd != 2) {
								t.Fatal("boundary citation lost its table row", cite)
							}
						}
					}
				})
			}
		})
	}
}
