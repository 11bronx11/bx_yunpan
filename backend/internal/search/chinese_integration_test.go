package search

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestElasticsearchChineseFulltext(t *testing.T) {
	endpoint := strings.TrimRight(os.Getenv("TEST_SEARCH_ES_ENDPOINT"), "/")
	if endpoint == "" {
		t.Skip("TEST_SEARCH_ES_ENDPOINT is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	es := NewElasticsearchIndex(endpoint, "", "", &http.Client{Timeout: 5 * time.Second})
	for _, legacy := range []bool{false, true} {
		target := "zh-probe-" + uuid.NewString()[:8]
		t.Run(target, func(t *testing.T) {
			if legacy {
				if err := es.do(ctx, http.MethodPut, "/chunks_"+target, map[string]any{"mappings": map[string]any{"properties": map[string]any{"content": map[string]any{"type": "text"}}}}, nil); err != nil {
					t.Fatal(err)
				}
			} else if err := es.PrepareEpoch(ctx, target, 2); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, name := range []string{"chunks_", "files_"} {
					_ = es.do(context.Background(), http.MethodDelete, "/"+name+target, nil, nil)
				}
			})
			contents := []string{
				"智能云盘支持中文搜索功能，使用 PostgreSQL 和 Elasticsearch。",
				"中文文档介绍索引，随后讨论搜索能力。",
				"中间的文字包含搜集资料和线索。",
				"ＰｏｓｔｇｒｅＳＱＬ 支持中文搜索。",
				"private English storage adapter reference",
				"已删除中文搜索",
			}
			ids := make([]uuid.UUID, len(contents))
			values := make([]ContentProjection, len(contents))
			for i, text := range contents {
				ids[i] = uuid.New()
				value := ContentProjection{TargetVersion: target, ChunkID: ids[i], ObjectID: uuid.New(), DocumentVersionID: uuid.New(), IndexEpoch: 1, Content: text, Tombstone: i == 5}
				values[i] = value
				var err error
				if legacy {
					// Reproduce a document produced by the old writer, with no
					// canonical search fields. New writers are tested below too.
					doc := map[string]any{"chunk_id": value.ChunkID, "object_id": value.ObjectID, "document_version_id": value.DocumentVersionID, "index_epoch": 1, "content": text, "tombstone": value.Tombstone}
					err = es.do(ctx, http.MethodPost, "/chunks_"+target+"/_update/"+value.ChunkID.String(), esUpdate(doc), nil)
				} else {
					err = es.UpsertContent(ctx, value)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := es.do(ctx, http.MethodPost, "/chunks_"+target+"/_refresh", nil, nil); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				query string
				want  []int
			}{
				{"中文搜索", []int{0, 3}},
				{"中文 搜索", []int{0, 1, 3}},
				{"云盘", []int{0}},
				{"盘", []int{0}},
				{"PostgreSQL 中文", []int{0, 3}},
				{"adapter storage", []int{4}},
				{"没有匹配", nil},
				{"，。！？", nil},
			} {
				// NFKC matching for existing fullwidth text requires canonical
				// fields; legacy documents keep their original English tokens.
				want := test.want
				if legacy && test.query == "PostgreSQL 中文" {
					want = []int{0}
				}
				t.Run(test.query, func(t *testing.T) {
					hits, err := es.SearchText(ctx, RetrievalRequest{TargetVersion: target, Query: test.query, Limit: 20})
					if err != nil {
						t.Fatal(err)
					}
					if len(hits) != len(want) {
						t.Fatalf("legacy=%v query=%q hits=%#v, want chunk indexes %v", legacy, test.query, hits, want)
					}
					for _, hit := range hits {
						found := false
						for _, i := range want {
							found = found || hit.ChunkID == ids[i]
						}
						if !found {
							t.Fatalf("unexpected candidate: %#v", hit)
						}
					}
					if test.query == "中文 搜索" && hits[0].ChunkID == ids[1] {
						t.Fatal("distant query terms ranked above an exact phrase")
					}
				})
			}
			if legacy {
				t.Run("canonical writes into an existing standard index", func(t *testing.T) {
					value := values[3]
					value.IndexEpoch = 2
					if err := es.UpsertContent(ctx, value); err != nil {
						t.Fatal(err)
					}
					if err := es.do(ctx, http.MethodPost, "/chunks_"+target+"/_refresh", nil, nil); err != nil {
						t.Fatal(err)
					}
					hits, err := es.SearchText(ctx, RetrievalRequest{TargetVersion: target, Query: "PostgreSQL 中文", Limit: 20})
					if err != nil || len(hits) != 2 {
						t.Fatalf("legacy field mapping changed canonical terms: %#v %v", hits, err)
					}
				})
			}
		})
	}
}
