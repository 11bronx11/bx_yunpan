package ai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

// Reranker scores already-retrieved documents against a query. Scores are in
// input order; callers own the top-k and diversity policy.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]float64, error)
}

// NewReranker keeps the feature opt-in. A disabled reranker is represented by
// nil so the normal RRF path has no extra provider call or allocation.
func NewReranker(cfg config.AI) Reranker {
	if !cfg.RerankEnabled {
		return nil
	}
	if cfg.Provider == "dashscope" {
		baseURL := cfg.RerankBaseURL
		if strings.TrimSpace(baseURL) == "" {
			baseURL = cfg.BaseURL
		}
		path := "/rerank"
		if strings.EqualFold(cfg.RerankModel, "qwen3-rerank") {
			path = "/reranks"
			// The Qwen3 DashScope-compatible endpoint uses compatible-api,
			// while chat/embedding use compatible-mode.
			baseURL = strings.Replace(baseURL, "/compatible-mode/v1", "/compatible-api/v1", 1)
		}
		baseURL = strings.TrimRight(baseURL, "/")
		if strings.HasSuffix(baseURL, path) {
			baseURL = strings.TrimSuffix(baseURL, path)
		}
		return &httpReranker{
			apiKey:  cfg.APIKey,
			baseURL: baseURL,
			model:   cfg.RerankModel,
			path:    path,
			client:  &http.Client{Timeout: cfg.RerankTimeout},
		}
	}
	return &fakeReranker{}
}

// fakeReranker makes the offline provider exercise the same second-stage
// control flow without contacting a model service.
type fakeReranker struct{}

func (*fakeReranker) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	result := make([]float64, len(docs))
	for index, doc := range docs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value := strings.ToLower(doc)
		if query != "" && strings.Contains(value, query) {
			result[index] = 1
			continue
		}
		for _, token := range strings.Fields(query) {
			if strings.Contains(value, token) {
				result[index] += 1
			}
		}
		// Keep ties deterministic so benchmark and interview examples are stable.
		digest := sha256.Sum256([]byte(doc))
		result[index] += float64(digest[0]) / 255000
	}
	return result, nil
}

type httpReranker struct {
	apiKey  string
	baseURL string
	model   string
	path    string
	client  *http.Client
}

type rerankResult struct {
	Index          int      `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
	Score          *float64 `json:"score"`
}

func (r *httpReranker) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if len(docs) == 0 {
		return []float64{}, nil
	}
	path := r.path
	if path == "" {
		path = "/rerank"
	}
	payload := map[string]any{"model": r.model, "query": query, "documents": docs, "top_n": len(docs)}
	if path == "/reranks" {
		payload["instruct"] = "Given a web search query, retrieve relevant passages that answer the query."
	} else {
		payload["return_documents"] = false
	}
	var raw json.RawMessage
	err := r.post(ctx, path, payload, &raw)
	if err != nil {
		return nil, err
	}
	var response struct {
		Results []rerankResult `json:"results"`
		Data    []rerankResult `json:"data"`
	}
	items := []rerankResult{}
	if err := json.Unmarshal(raw, &items); err == nil {
		// Text Embeddings Inference and several local BGE/Qwen servers return a
		// bare array of {index, score}; hosted APIs commonly wrap it in results.
	} else if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode reranker response: %w", err)
	} else {
		items = response.Results
		if len(items) == 0 {
			items = response.Data
		}
	}
	values := make([]float64, len(docs))
	seen := make([]bool, len(docs))
	if len(items) != len(docs) {
		return nil, fmt.Errorf("reranker returned %d scores for %d documents", len(items), len(docs))
	}
	for _, item := range items {
		if item.Index < 0 || item.Index >= len(docs) || seen[item.Index] {
			return nil, errors.New("reranker returned invalid document index")
		}
		seen[item.Index] = true
		if item.RelevanceScore != nil {
			values[item.Index] = *item.RelevanceScore
		} else if item.Score != nil {
			values[item.Index] = *item.Score
		} else {
			return nil, errors.New("reranker score is missing")
		}
		if math.IsNaN(values[item.Index]) || math.IsInf(values[item.Index], 0) {
			return nil, errors.New("reranker score is not finite")
		}
	}
	for _, value := range seen {
		if !value {
			return nil, errors.New("reranker omitted a document score")
		}
	}
	return values, nil
}

func (r *httpReranker) post(ctx context.Context, path string, payload, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if strings.TrimSpace(r.apiKey) != "" {
		request.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read reranker response: %w", err)
	}
	if len(responseBody) > maxProviderResponseBytes {
		return errors.New("reranker response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("reranker returned status %d", response.StatusCode)
	}
	return json.Unmarshal(responseBody, target)
}

type rerankOrder struct {
	index int
	score float64
}

func orderByRerank(scores []float64) []int {
	ordered := make([]rerankOrder, len(scores))
	for index, score := range scores {
		ordered[index] = rerankOrder{index: index, score: score}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].score > ordered[j].score })
	result := make([]int, len(ordered))
	for index, item := range ordered {
		result[index] = item.index
	}
	return result
}

var _ Reranker = (*fakeReranker)(nil)
var _ Reranker = (*httpReranker)(nil)
