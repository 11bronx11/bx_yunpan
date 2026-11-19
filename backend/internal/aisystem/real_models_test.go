package aisystem

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// Optional bounded external-provider check: one vision, document embedding,
// summary, query embedding and grounded answer. Credentials exist only in env.
func TestRealModelImagePipeline(t *testing.T) {
	realModelImagePipeline(t, false)
}

func TestDirectRealModelImagePipeline(t *testing.T) {
	realModelImagePipeline(t, true)
}

func realModelImagePipeline(t *testing.T, direct bool) {
	if os.Getenv("TEST_REAL_AI_CONFIG") == "" {
		t.Skip("explicit real model configuration required")
	}
	var cfg config.AI
	if json.Unmarshal([]byte(os.Getenv("TEST_REAL_AI_CONFIG")), &cfg) != nil || cfg.Provider != "dashscope" || cfg.APIKey == "" {
		t.Fatal("invalid real model configuration")
	}
	cfg.RequestTimeout = 45 * time.Second
	if model := os.Getenv("TEST_REAL_VISION_MODEL"); model != "" {
		cfg.VisionModel = model
	}
	provider := ai.NewProvider(cfg)
	db, store := databases(t)
	if direct {
		store.ProtocolVersion = 3
		if err := store.DB.Exec("SET bx.coordination_protocol='v3'; UPDATE control_settings SET value='v3' WHERE key='coordination_protocol'").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("SET bx.ai_protocol='v3'").Error; err != nil {
			t.Fatal(err)
		}
	}
	in := testInput()
	in.Intent.MediaType = "image/png"
	in.Profile.EmbeddingProvider = cfg.Provider
	in.Profile.EmbeddingModel = cfg.EmbeddingModel
	in.Profile.SummaryProvider = cfg.Provider
	in.Profile.SummaryModel = cfg.ChatModel
	in.Profile.VisionProvider = cfg.Provider
	in.Profile.VisionModel = cfg.VisionModel
	canvas := image.NewRGBA(image.Rect(0, 0, 420, 160))
	for y := 0; y < 160; y++ {
		for x := 0; x < 420; x++ {
			canvas.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	glyphs := map[rune][]string{
		'B': {"11110", "10001", "10001", "11110", "10001", "10001", "11110"},
		'X': {"10001", "10001", "01010", "00100", "01010", "10001", "10001"},
		'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
		'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
		'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
		'9': {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	}
	for n, r := range "BX7319" {
		for y, row := range glyphs[r] {
			for x, pixel := range row {
				if pixel == '1' {
					for dy := 0; dy < 8; dy++ {
						for dx := 0; dx < 8; dx++ {
							canvas.SetRGBA(50+n*48+x*8+dx, 45+y*8+dy, color.RGBA{0, 0, 0, 255})
						}
					}
				}
			}
		}
	}

	var buf bytes.Buffer
	if e := png.Encode(&buf, canvas); e != nil {
		t.Fatal(e)
	}
	in.Intent.Size = int64(buf.Len())
	in.Intent.ContentHash = aiprotocol.Hash(buf.Bytes())
	seedObject(t, db, in)
	if direct {
		if err := db.Exec("UPDATE ai_objects SET writer_protocol=3").Error; err != nil {
			t.Fatal(err)
		}
	}
	object := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(buf.Bytes()) }))
	defer object.Close()
	file := &corepb.FileRef{FileId: uuid.NewString(), ObjectId: in.Intent.ObjectID.String(), ContentGeneration: 1, MimeType: "image/png", Name: "equipment.png"}
	core := &parityCore{scopeClient: scopeClient{file: file}, object: &corepb.ProcessingResponse{ReadUrl: object.URL, Size: in.Intent.Size, Sha256: in.Intent.ContentHash}, files: []*corepb.FileRef{file}}
	c := &Computer{DB: db, Core: core, Vision: provider, Embedding: provider, Summary: provider, Profile: in.Profile, HTTP: object.Client(), SummaryCalls: 8}
	p := &Publisher{DB: db, Control: store}
	compute := func(kind string, input aiprotocol.Input) aiprotocol.Output {
		t.Helper()
		if direct {
			task := directTask(kind, input)
			value, err := c.ComputeValue(context.Background(), task)
			if err != nil {
				t.Fatalf("real direct model computation %s failed: %s", kind, safeRealError(err))
			}
			completion, err := p.ApplyDirect(context.Background(), task, value)
			if err != nil || completion.Outcome != "succeeded" {
				t.Fatalf("real direct model stage %s failed: %s", kind, safeRealError(err))
			}
			refs, err := decode[aiprotocol.Published](completion.Refs)
			if err != nil {
				t.Fatal("direct publication references invalid")
			}
			return aiprotocol.Output{OutputID: refs.OutputID}
		}
		task := claim(t, store, kind, input)
		body, e := c.Compute(context.Background(), task)
		if e != nil {
			t.Fatalf("real model stage %s failed: %s", kind, safeRealError(e))
		}
		result, e := store.SaveResult(context.Background(), task, body)
		if e != nil {
			t.Fatal("result acceptance failed")
		}
		publish(t, p, task, result)
		out, e := decode[aiprotocol.Output](body)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	in.SourceID = compute("ai.source.prepare", in).OutputID
	var blocks []string
	if e := db.Table("ai_published_blocks").Where("source_id=?", in.SourceID).Pluck("text", &blocks).Error; e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(strings.Join(blocks, " "), "BX7319") {
		t.Fatal("vision failed to recover the image's equipment ID")
	}
	summaryIn := in
	in.ContentVersionID = compute("ai.text.build", in).OutputID
	compute("ai.embedding.generate", in)
	compute("ai.summary.generate", summaryIn)
	q := &Query{DB: db, Core: core, Provider: provider, Profile: in.Profile, EmbeddingCache: platformcache.NewEmbeddingStore(nil, time.Minute, 8)}
	search, e := q.Search(context.Background(), &pbv2.SearchRequest{Query: "BX7319", Mode: "fulltext"})
	if e != nil || len(search.Hits) != 1 {
		t.Fatal("real vision text not searchable")
	}
	semantic, e := q.Search(context.Background(), &pbv2.SearchRequest{Query: "图片中的设备编号是什么？", Mode: "semantic"})
	if e != nil || len(semantic.Hits) != 1 || semantic.Retrieval.VectorBackend != "postgres" {
		t.Fatal("actual query embedding not retrievable")
	}
	// The existing scope fixture is authenticated independently in full-chain tests.
	grant := testAllScopeGrant(t)
	answer, e := q.Ask(context.Background(), &pbv2.AskRequest{Grant: grant, Question: "图片中的设备编号是什么？"})
	if e != nil {
		t.Fatalf("real answer failed: %s", safeRealError(e))
	}
	if !strings.Contains(answer.Answer, "BX7319") || len(answer.Citations) == 0 || answer.Citations[0].FileId != file.FileId {
		t.Fatal("real answer lost the known fact or versioned evidence")
	}
	if cfg.RerankEnabled {
		scores, e := ai.NewReranker(cfg).Rerank(context.Background(), "设备编号", []string{"设备编号 BX7319，预算已批准", "无关的天气预报"})
		if e != nil || len(scores) != 2 || scores[0] <= scores[1] {
			t.Fatal("real reranker failed known relevant/irrelevant pair")
		}
	}
	t.Log("actual vision -> AI PG source -> parent/child -> actual embedding + summary -> PG fulltext/semantic -> actual answer + versioned citation: PASS")
}

func safeRealError(err error) string {
	// Provider errors may include a transport URL. Keep test artifacts bounded.
	var build *modelBuildError
	if errors.As(err, &build) {
		return safeRealError(build.cause)
	}
	if ai.IsProviderPermanent(err) {
		return err.Error()
	}
	if ai.IsParseUnsupported(err) {
		return "unsupported"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return urlErr.Err.Error()
		}
		return "network timeout"
	}
	return "dependency or provider failure"
}

func testAllScopeGrant(t *testing.T) string {
	t.Helper()
	_, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	expires := time.Now().Add(2 * time.Minute).Unix()
	grant, e := serviceauth.Sign(key, "core-retrieval-v1", expires, aiprotocol.ScopeGrant{Principal: uuid.New(), Scope: aiprotocol.Scope{Mode: "all"}, Action: "retrieve", ExpiresAt: expires})
	if e != nil {
		t.Fatal(e)
	}
	return grant
}
