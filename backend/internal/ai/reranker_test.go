package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func TestFakeRerankerReturnsInputAlignedScores(t *testing.T) {
	reranker := &fakeReranker{}
	scores, err := reranker.Rerank(context.Background(), "权限校验", []string{
		"向量检索说明",
		"权限校验必须在 SQL 中执行",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[1] <= scores[0] {
		t.Fatalf("expected query-matching document to rank higher: %v", scores)
	}
}

func TestOrderByRerank(t *testing.T) {
	ordered := orderByRerank([]float64{0.2, 0.9, 0.5})
	if len(ordered) != 3 || ordered[0] != 1 || ordered[1] != 2 || ordered[2] != 0 {
		t.Fatalf("unexpected order: %v", ordered)
	}
}

func TestNewRerankerIsOptIn(t *testing.T) {
	cfg := config.AI{Provider: "fake", RerankEnabled: false}
	if reranker := NewReranker(cfg); reranker != nil {
		t.Fatalf("disabled reranker must be nil, got %T", reranker)
	}
	cfg.RerankEnabled = true
	if reranker := NewReranker(cfg); reranker == nil {
		t.Fatal("fake provider should create a local reranker")
	}
}

func TestRerankRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&fakeReranker{}).Rerank(ctx, "q", []string{strings.Repeat("x", 4)})
	if err == nil {
		t.Fatal("expected canceled context error")
	}
}

func TestHTTPRerankerRestoresScoresByIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/rerank" || request.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected reranker request: %s %s", request.URL.Path, request.Header.Get("Authorization"))
		}
		var payload struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "test-model" || payload.Query != "q" || len(payload.Documents) != 2 {
			t.Fatalf("unexpected payload: %+v", payload)
		}
		_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}]}`))
	}))
	defer server.Close()

	reranker := &httpReranker{apiKey: "test-key", baseURL: server.URL, model: "test-model", client: server.Client()}
	scores, err := reranker.Rerank(context.Background(), "q", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0] != 0.1 || scores[1] != 0.9 {
		t.Fatalf("scores are not input-aligned: %v", scores)
	}
}

func TestHTTPRerankerAcceptsBareScoreArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = w.Write([]byte(`[{"index":0,"score":0},{"index":1,"score":0.7}]`))
	}))
	defer server.Close()

	reranker := &httpReranker{baseURL: server.URL, client: server.Client()}
	scores, err := reranker.Rerank(context.Background(), "q", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0] != 0 || scores[1] != 0.7 {
		t.Fatalf("unexpected bare-array scores: %v", scores)
	}
}

func TestQwenDashScopeRerankerUsesCompatibleRerankContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/reranks" {
			t.Fatalf("unexpected qwen rerank path: %s", request.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "qwen3-rerank" || payload["query"] != "q" || payload["return_documents"] != nil {
			t.Fatalf("unexpected qwen rerank payload: %+v", payload)
		}
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.8}]}`))
	}))
	defer server.Close()

	reranker := &httpReranker{baseURL: server.URL, path: "/reranks", model: "qwen3-rerank", client: server.Client()}
	scores, err := reranker.Rerank(context.Background(), "q", []string{"doc"})
	if err != nil || len(scores) != 1 || scores[0] != 0.8 {
		t.Fatalf("unexpected qwen rerank result: scores=%v err=%v", scores, err)
	}
}

func TestNewQwenDashScopeRerankerUsesCompatibleAPIBase(t *testing.T) {
	raw := NewReranker(config.AI{
		Provider: "dashscope", RerankEnabled: true, BaseURL: "https://example.test/compatible-mode/v1",
		RerankModel: "qwen3-rerank",
	}).(*httpReranker)
	if raw.path != "/reranks" || raw.baseURL != "https://example.test/compatible-api/v1" {
		t.Fatalf("unexpected qwen dashscope endpoint: base=%q path=%q", raw.baseURL, raw.path)
	}
}
