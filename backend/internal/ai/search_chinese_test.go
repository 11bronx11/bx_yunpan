package ai

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	searchindex "github.com/11bronx11/bx_yunpan/backend/internal/search"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestPostgresChineseFulltext(t *testing.T) {
	db := versionDB(t)
	rollback := errors.New("rollback Chinese search fixtures")
	err := db.Transaction(func(tx *gorm.DB) error {
		owner, stranger, folder := uuid.New(), uuid.New(), uuid.New()
		for _, id := range []uuid.UUID{owner, stranger} {
			if err := tx.Exec(`INSERT INTO users(id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'test')`, id, id.String(), id.String()).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec(`INSERT INTO folders(id, owner_id, name, name_normalized) VALUES (?, ?, '/', '/')`, folder, owner).Error; err != nil {
			return err
		}
		contents := []string{
			"智能云盘支持中文搜索功能，使用 PostgreSQL 和 Elasticsearch。",
			"中文文档介绍索引，随后讨论搜索能力。",
			"中间的文字包含搜集资料和线索。", // scattered characters must not suffice
			"ＰｏｓｔｇｒｅＳＱＬ 支持中文搜索。",
			"private English storage adapter reference",
			"私密中文搜索", // another owner's document
			"已删除中文搜索",
			"旧版本中文搜索",
		}
		files := make([]uuid.UUID, len(contents))
		chunks := make([]uuid.UUID, len(contents))
		for i, content := range contents {
			objectID, documentID, versionID, chunkID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			files[i] = uuid.New()
			chunks[i] = chunkID
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
			if err := tx.Exec(`INSERT INTO file_objects(id, sha256, size_bytes, mime_type, bucket, object_key, status)
				VALUES (?, ?, ?, 'text/plain', 'test', ?, 'ready')`, objectID, digest, len(content), objectID.String()).Error; err != nil {
				return err
			}
			fileOwner := owner
			if i == 5 {
				fileOwner = stranger
			}
			if err := tx.Exec(`INSERT INTO file_entries(id, owner_id, folder_id, object_id, name, name_normalized, deleted_at)
				VALUES (?, ?, ?, ?, ?, ?, CASE WHEN ? THEN now() ELSE NULL END)`, files[i], fileOwner, folder, objectID, files[i].String(), files[i].String(), i == 6).Error; err != nil {
				return err
			}
			if err := tx.Exec(`INSERT INTO ai_documents(id, object_id, status, pipeline_version, model_version)
				VALUES (?, ?, 'indexed', 'test', 'test')`, documentID, objectID).Error; err != nil {
				return err
			}
			if err := tx.Exec(`INSERT INTO ai_document_versions(id, document_id, object_id, pipeline_version, model_version, generation, status)
				VALUES (?, ?, ?, 'test', 'test', 1, 'indexed')`, versionID, documentID, objectID).Error; err != nil {
				return err
			}
			if err := tx.Exec(`UPDATE ai_documents SET active_version_id = ? WHERE id = ?`, versionID, documentID).Error; err != nil {
				return err
			}
			if i == 7 {
				// A retired chunk is never eligible, even with matching content.
				oldVersion := uuid.New()
				if err := tx.Exec(`INSERT INTO ai_document_versions(id, document_id, object_id, pipeline_version, model_version, generation, status)
					VALUES (?, ?, ?, 'test', 'test', 2, 'indexed')`, oldVersion, documentID, objectID).Error; err != nil {
					return err
				}
				versionID = oldVersion
			}
			if err := tx.Exec(`INSERT INTO ai_chunks(id, document_id, document_version_id, ordinal, content, embedding)
				VALUES (?, ?, ?, 0, ?, CAST(? AS vector))`, chunkID, documentID, versionID, content, vectorLiteral(make([]float32, 1024))).Error; err != nil {
				return err
			}
		}
		service := &Service{db: tx, provider: &fakeProvider{dimension: 1024}}
		for _, test := range []struct {
			query string
			want  []int
		}{
			{"中文搜索", []int{0, 3}},
			{"中文 搜索", []int{0, 1, 3}},
			{"云盘", []int{0}},
			{"盘", []int{0}},
			{"postgresql 中文", []int{0, 3}},
			{"ＰＯＳＴＧＲＥＳＱＬ", []int{0, 3}},
			{"adapter storage", []int{4}},
			{"没有匹配", nil},
			{"，。！？", nil},
		} {
			t.Run(test.query, func(t *testing.T) {
				hits, err := service.Search(context.Background(), owner, SearchInput{Query: test.query, Mode: "fulltext", Limit: 20})
				if err != nil {
					t.Fatal(err)
				}
				want := map[uuid.UUID]bool{}
				for _, i := range test.want {
					want[files[i]] = true
				}
				if len(hits) != len(want) {
					t.Fatalf("hits=%#v, want file indexes %v", hits, test.want)
				}
				for _, hit := range hits {
					if !want[hit.File["id"].(uuid.UUID)] {
						t.Fatalf("unexpected file: %#v", hit)
					}
					if len(hit.Citations) != 1 || hit.Citations[0].Excerpt == "" {
						t.Fatalf("missing source excerpt: %#v", hit)
					}
				}
				if test.query == "中文 搜索" && hits[0].File["id"].(uuid.UUID) == files[1] {
					t.Fatal("distant query terms ranked above adjacent terms")
				}
			})
		}
		t.Run("file and MIME scopes", func(t *testing.T) {
			hits, err := service.Search(context.Background(), owner, SearchInput{Query: "中文搜索", Mode: "fulltext", FileIDs: []uuid.UUID{files[0]}, Limit: 20})
			if err != nil || len(hits) != 1 || hits[0].File["id"].(uuid.UUID) != files[0] {
				t.Fatalf("file scope: hits=%#v err=%v", hits, err)
			}
			hits, err = service.Search(context.Background(), owner, SearchInput{Query: "中文搜索", Mode: "fulltext", MimeTypes: []string{"application/pdf"}, Limit: 20})
			if err != nil || len(hits) != 0 {
				t.Fatalf("MIME scope: hits=%#v err=%v", hits, err)
			}
		})
		t.Run("ES outage falls back to Chinese PG search", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			service.WithRetrieval(searchindex.NewElasticsearchIndex(server.URL, "", "", server.Client()), config.Search{ReadBackend: "es_qdrant", ExternalFallback: true})
			hits, err := service.Search(context.Background(), owner, SearchInput{Query: "云盘", Mode: "fulltext", Limit: 20})
			if err != nil || len(hits) != 1 || hits[0].File["id"].(uuid.UUID) != files[0] {
				t.Fatalf("Chinese fallback: hits=%#v err=%v", hits, err)
			}
		})
		t.Run("external scope model order and lag", func(t *testing.T) {
			var external []searchindex.RetrievalCandidate
			if err := tx.Raw(`SELECT c.id AS chunk_id, d.object_id, c.document_version_id FROM ai_chunks c JOIN ai_documents d ON d.id = c.document_id`).Scan(&external).Error; err != nil {
				t.Fatal(err)
			}
			for i := range external {
				for rank, id := range chunks {
					if external[i].ChunkID == id {
						external[i].Score = float64(rank + 1)
					}
				}
			}
			service.WithPipelineVersion("test").WithRetrieval(staticSearchBackend{candidates: external}, config.Search{ReadBackend: "es_qdrant"})
			input := SearchInput{Query: "中文搜索", Mode: "fulltext", Limit: 20}
			rows, err := service.queryExternalCandidates(context.Background(), owner, input, "fulltext", "")
			if err != nil || len(rows) != 5 || rows[0].FileID != files[4] {
				t.Fatalf("external scope/order: %+v %v", rows, err)
			}
			input.MimeTypes = []string{"application/pdf"}
			rows, err = service.queryExternalCandidates(context.Background(), owner, input, "fulltext", "")
			if err != nil || len(rows) != 0 {
				t.Fatalf("external MIME mismatch: %+v %v", rows, err)
			}
			input.MimeTypes = nil
			rows, err = service.queryExternalCandidates(context.Background(), owner, input, "semantic", "[1,0]")
			if err != nil || len(rows) != 0 {
				t.Fatalf("incompatible model accepted: %+v %v", rows, err)
			}
			if err := tx.Exec(`UPDATE ai_document_versions SET model_version = ? WHERE id = (SELECT c.document_version_id FROM ai_chunks c WHERE c.id = ?)`, service.provider.ModelVersion(), chunks[0]).Error; err != nil {
				t.Fatal(err)
			}
			rows, err = service.queryExternalCandidates(context.Background(), owner, input, "semantic", "[1,0]")
			if err != nil || len(rows) != 1 || rows[0].FileID != files[0] {
				t.Fatalf("compatible model lost: %+v %v", rows, err)
			}
			service.WithRetrieval(staticSearchBackend{}, config.Search{ReadBackend: "es_qdrant", ExternalFallback: true})
			hits, err := service.Search(context.Background(), owner, SearchInput{Query: "云盘", Mode: "fulltext", Limit: 1})
			if err != nil || len(hits) != 1 || hits[0].File["id"].(uuid.UUID) != files[0] {
				t.Fatalf("empty external result hid PG content: %+v %v", hits, err)
			}
			for _, candidate := range external {
				if candidate.ChunkID != chunks[5] {
					continue
				}
				service.WithRetrieval(staticSearchBackend{candidates: []searchindex.RetrievalCandidate{candidate}}, config.Search{ReadBackend: "es_qdrant", ExternalFallback: true})
			}
			hits, err = service.Search(context.Background(), owner, SearchInput{Query: "云盘", Mode: "fulltext", Limit: 1})
			if err != nil || len(hits) != 1 || hits[0].File["id"].(uuid.UUID) != files[0] {
				t.Fatalf("another owner's candidates starved recall: %+v %v", hits, err)
			}
		})
		// The migration builds an actual GIN access path, rather than scanning
		// source text with a wildcard. Disable seqscan only for this plan probe.
		if err := tx.Exec(`SET LOCAL enable_seqscan = off`).Error; err != nil {
			return err
		}
		var plans []string
		if err := tx.Raw(`EXPLAIN SELECT id FROM ai_chunks WHERE content_search_tsv @@ plainto_tsquery('simple', ai_search_terms('中文搜索', true))`).Scan(&plans).Error; err != nil {
			return err
		}
		found := false
		for _, plan := range plans {
			if strings.Contains(plan, "idx_ai_chunks_search_fts") {
				found = true
			}
		}
		if !found {
			t.Errorf("GIN index absent from plan: %v", plans)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
}

type staticSearchBackend struct {
	candidates []searchindex.RetrievalCandidate
}

func (s staticSearchBackend) SearchText(context.Context, searchindex.RetrievalRequest) ([]searchindex.RetrievalCandidate, error) {
	return s.candidates, nil
}
func (s staticSearchBackend) SearchVector(context.Context, searchindex.RetrievalRequest, []float32) ([]searchindex.RetrievalCandidate, error) {
	return s.candidates, nil
}
func (staticSearchBackend) Health(context.Context) error { return nil }
