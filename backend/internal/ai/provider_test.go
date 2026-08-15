package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderMapsFreeTierQuotaError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"free quota exhausted","code":"AllocationQuota.FreeTierOnly"}}`))
	}))
	defer server.Close()

	provider := &dashScopeProvider{baseURL: server.URL, client: server.Client()}
	err := provider.post(context.Background(), "/embeddings", map[string]any{"input": []string{"test"}}, &struct{}{})
	if !errors.Is(err, errProviderQuota) {
		t.Fatalf("expected quota error, got %v", err)
	}
}

func TestProviderBoundsGeneratedSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"` + strings.Repeat("项", maxSummaryRunes+20) + `\",\"tags\":[],\"language\":\"zh\"}"}}]}`))
	}))
	defer server.Close()

	provider := &dashScopeProvider{baseURL: server.URL, chatModel: "test-chat", client: server.Client()}
	insight, err := provider.Enrich(context.Background(), "document")
	if err != nil {
		t.Fatal(err)
	}
	if length := len([]rune(insight.Summary)); length != maxSummaryRunes {
		t.Fatalf("expected summary length %d, got %d", maxSummaryRunes, length)
	}
}

func TestProviderMarksInvalidRequestAsPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid model","code":"InvalidParameter"}}`))
	}))
	defer server.Close()

	provider := &dashScopeProvider{baseURL: server.URL, client: server.Client()}
	err := provider.post(context.Background(), "/embeddings", map[string]any{"input": []string{"test"}}, &struct{}{})
	if !errors.Is(err, errProviderPermanent) {
		t.Fatalf("expected permanent provider error, got %v", err)
	}
	if errors.Is(err, errProviderQuota) {
		t.Fatalf("invalid request must not be reported as quota exhaustion: %v", err)
	}
}

func TestParseAnswerResultFiltersUnsupportedCitations(t *testing.T) {
	evidence := []Evidence{{ID: "chunk-1", FileName: "项目.pdf", Content: "接口联调"}}
	result, err := parseAnswerResult(`{"answer":"需要完成接口联调。","grounded":true,"citations":["chunk-1","unknown","chunk-1"]}`, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Grounded || len(result.CitationIDs) != 1 || result.CitationIDs[0] != "chunk-1" {
		t.Fatalf("unexpected grounded result: %+v", result)
	}
}

func TestParseAnswerResultDropsCitationsWhenUngrounded(t *testing.T) {
	evidence := []Evidence{{ID: "chunk-1", FileName: "项目.pdf", Content: "接口联调"}}
	result, err := parseAnswerResult(`{"answer":"当前证据不足，无法回答。","grounded":false,"citations":["chunk-1"]}`, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if result.Grounded || len(result.CitationIDs) != 0 {
		t.Fatalf("ungrounded answer retained citations: %+v", result)
	}
}

func TestParseAnswerResultTreatsNonJSONAsUngrounded(t *testing.T) {
	result, err := parseAnswerResult("当前证据不足，无法回答。", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Grounded || len(result.CitationIDs) != 0 || result.Answer == "" {
		t.Fatalf("unexpected fallback result: %+v", result)
	}
}

func TestDashScopeAnswerParsesGroundingContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"answer\":\"接口联调需要在发布前完成。\",\"grounded\":true,\"citations\":[\"chunk-1\",\"missing\"]}"}}]}`))
	}))
	defer server.Close()

	provider := &dashScopeProvider{baseURL: server.URL, chatModel: "test-chat", client: server.Client()}
	result, err := provider.Answer(context.Background(), "发布前要做什么？", []Evidence{{ID: "chunk-1", FileName: "发布清单.md", Content: "完成接口联调"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Grounded || len(result.CitationIDs) != 1 || result.CitationIDs[0] != "chunk-1" {
		t.Fatalf("unexpected answer result: %+v", result)
	}
}
