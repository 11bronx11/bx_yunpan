package ai_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/parserengine"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/search"
	"github.com/11bronx11/bx_yunpan/backend/internal/textsearch"
	"github.com/google/uuid"
	"golang.org/x/text/encoding/simplifiedchinese"
	textunicode "golang.org/x/text/encoding/unicode"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// This gate uses both actual parsers, actual source chunks, and the public
// Search service against PG and ES. ES fallback is disabled so PG cannot mask
// an incompatible external index/query representation.
func TestChineseParserToSearchPipeline(t *testing.T) {
	dsn, endpoint := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("TEST_SEARCH_ES_ENDPOINT")
	if dsn == "" || endpoint == "" || os.Getenv("TEST_CHINESE_PARSER_WORKER") != "1" {
		t.Skip("TEST_POSTGRES_DSN, TEST_SEARCH_ES_ENDPOINT and TEST_CHINESE_PARSER_WORKER=1 are required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	workerDir, err := filepath.Abs("../../parser_worker")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := parserengine.Start(ctx, address, workerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	cfg := config.AI{Provider: "fake", Dimension: 1024, RequestTimeout: time.Second}
	for _, parser := range []struct {
		name    string
		gateway ai.ParserGateway
	}{
		{"builtin", ai.NewBuiltinParser(ai.NewProvider(cfg))},
		{"python", worker},
	} {
		t.Run(parser.name, func(t *testing.T) {
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			es := search.NewElasticsearchIndex(endpoint, "", "", &http.Client{Timeout: 5 * time.Second})
			target := "zh-chain-" + uuid.NewString()[:8]
			if err := es.PrepareEpoch(ctx, target, 1024); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, name := range []string{"chunks_", "files_"} {
					request, _ := http.NewRequest(http.MethodDelete, strings.TrimRight(endpoint, "/")+"/"+name+target, nil)
					if response, err := http.DefaultClient.Do(request); err == nil {
						response.Body.Close()
					}
				}
			})
			owner, folder := uuid.New(), uuid.New()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(tx.Exec(`INSERT INTO users(id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'test')`, owner, owner.String(), owner.String()).Error)
			must(tx.Exec(`INSERT INTO folders(id, owner_id, name, name_normalized) VALUES (?, ?, '/', '/')`, folder, owner).Error)
			const common = "中华人民共和国支持中文搜索，使用ＰｏｓｔｇｒｅＳＱＬ和ＥＳ；①号ＡＩ文件保持原文。智能云盘。"
			gbText := strings.Repeat("甲", 61) + common
			gb, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(gbText))
			must(err)
			utf16, err := textunicode.UTF16(textunicode.BigEndian, textunicode.UseBOM).NewEncoder().Bytes([]byte(common))
			must(err)
			cases := []struct {
				name, mime, original string
				data                 []byte
			}{
				{"gb18030", "text/plain", gbText, gb},
				{"utf16be", "text/plain", common, utf16},
				{"utf8", "text/plain", common, []byte(common)},
				{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", common, chineseDOCX(t, common)},
				{"distractor", "text/plain", "中间的文字讲述搜集线索。", []byte("中间的文字讲述搜集线索。")},
			}
			chunker := ai.NewChunkingParser(parser.gateway, rag.ChunkOptions{TokenBudget: 64, Overlap: 12})
			legacyJSON, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(`{"名称":"中文"}`))
			must(err)
			for _, invalid := range []struct {
				name, mime string
				data       []byte
			}{
				{"invalid Unicode", "text/plain", []byte{0xff, 0xfe, 0}},
				{"binary control in long text", "text/plain", []byte(strings.Repeat("中文", 100) + "\x01")},
				{"legacy JSON", "application/json", legacyJSON},
				{"invalid JSON", "application/json", []byte(`{"名称":}`)},
				{"empty text", "text/plain", []byte(" \n\t")},
			} {
				t.Run("reject "+invalid.name, func(t *testing.T) {
					_, err := chunker.Parse(ctx, ai.ParseRequest{ObjectID: uuid.New(), MediaType: invalid.mime, ObjectAddress: ai.ObjectReadAddress{Data: invalid.data}})
					if err == nil || ai.IsParseRetryable(err) {
						t.Fatalf("corrupt source was accepted or classified as retryable: %v", err)
					}
				})
			}
			fileNames := map[uuid.UUID]string{}
			for _, sample := range cases {
				fileID, objectID, documentID, versionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
				fileNames[fileID] = sample.name
				parsed, err := chunker.Parse(ctx, ai.ParseRequest{ObjectID: objectID, MediaType: sample.mime, ObjectAddress: ai.ObjectReadAddress{Data: sample.data}})
				must(err)
				if parsed.ChunksTruncated || parsed.ChunkerVersion != rag.DefaultChunker || len(parsed.Chunks) == 0 {
					t.Fatalf("invalid parsed chunks for %s: %#v", sample.name, parsed)
				}
				var original strings.Builder
				for _, section := range parsed.Sections {
					original.WriteString(strings.TrimSpace(section.Text))
				}
				if original.String() != sample.original {
					t.Fatalf("%s source changed before chunking: %q", sample.name, original.String())
				}
				digest := fmt.Sprintf("%x", sha256.Sum256(sample.data))
				must(tx.Exec(`INSERT INTO file_objects(id, sha256, size_bytes, mime_type, bucket, object_key, status) VALUES (?, ?, ?, ?, 'probe', ?, 'ready')`, objectID, digest, len(sample.data), sample.mime, objectID.String()).Error)
				must(tx.Exec(`INSERT INTO file_entries(id, owner_id, folder_id, object_id, name, name_normalized) VALUES (?, ?, ?, ?, ?, ?)`, fileID, owner, folder, objectID, sample.name, sample.name).Error)
				must(tx.Exec(`INSERT INTO ai_documents(id, object_id, status, pipeline_version, model_version) VALUES (?, ?, 'indexed', 'probe', 'probe')`, documentID, objectID).Error)
				must(tx.Exec(`INSERT INTO ai_document_versions(id, document_id, object_id, pipeline_version, model_version, generation, status) VALUES (?, ?, ?, 'probe', 'probe', 1, 'indexed')`, versionID, documentID, objectID).Error)
				must(tx.Exec(`UPDATE ai_documents SET active_version_id = ? WHERE id = ?`, versionID, documentID).Error)
				for i, chunk := range parsed.Chunks {
					if !strings.Contains(sample.original, chunk.Content) || chunk.TokenCount > 64 {
						t.Fatalf("%s source/budget broken by chunking: %q", sample.name, chunk.Content)
					}
					chunkID := uuid.New()
					vector := "[" + strings.Repeat("0,", 1023) + "0]"
					must(tx.Exec(`INSERT INTO ai_chunks(id, document_id, document_version_id, ordinal, content, embedding) VALUES (?, ?, ?, ?, ?, CAST(? AS vector))`, chunkID, documentID, versionID, i, chunk.Content, vector).Error)
					var pgTerms string
					must(tx.Raw("SELECT ai_search_terms(?)", chunk.Content).Scan(&pgTerms).Error)
					if pgTerms != textsearch.IndexText(chunk.Content) {
						t.Fatal("PG and ES writers disagree on a parsed source chunk")
					}
					must(es.UpsertContent(ctx, search.ContentProjection{TargetVersion: target, ChunkID: chunkID, ObjectID: objectID, DocumentVersionID: versionID, Ordinal: i, IndexEpoch: 1, Content: chunk.Content}))
				}
			}
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/chunks_"+target+"/_refresh", nil)
			response, err := http.DefaultClient.Do(request)
			must(err)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("ES refresh returned %d", response.StatusCode)
			}
			pg := ai.NewService(tx, nil, nil, nil, cfg)
			external := ai.NewService(tx, nil, nil, nil, cfg).WithRetrieval(es, config.Search{ReadBackend: "es_qdrant", IndexEpoch: target, ExternalFallback: false})
			for _, query := range []string{"共和国", "中文搜索", "中文 搜索", "PostgreSQL", "ＰＯＳＴＧＲＥＳＱＬ", "es", "①号", "1号", "ai", "盘", "不存在的内容", "，。！？"} {
				t.Run(query, func(t *testing.T) {
					want := []string{"docx", "gb18030", "utf16be", "utf8"}
					if query == "不存在的内容" || query == "，。！？" {
						want = nil
					}
					for _, backend := range []struct {
						name    string
						service *ai.Service
					}{{"pg", pg}, {"es", external}} {
						hits, err := backend.service.Search(ctx, owner, ai.SearchInput{Query: query, Mode: "fulltext", Limit: 100})
						if err != nil {
							t.Fatal(err)
						}
						var got []string
						for _, hit := range hits {
							got = append(got, fileNames[hit.File["id"].(uuid.UUID)])
						}
						sort.Strings(got)
						if strings.Join(got, ",") != strings.Join(want, ",") {
							t.Fatalf("%s query=%q got=%v want=%v", backend.name, query, got, want)
						}
					}
				})
			}
		})
	}
}

func chineseDOCX(t *testing.T, text string) []byte {
	t.Helper()
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	for name, xml := range map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels":         `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>中华人民</w:t></w:r><w:r><w:t>` + strings.TrimPrefix(text, "中华人民") + `</w:t></w:r></w:p></w:body></w:document>`,
	} {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(xml)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
