package ai

import (
	"context"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

type countingEmbeddingProvider struct {
	calls int
}

func (p *countingEmbeddingProvider) ModelVersion() string { return "cache-test" }
func (p *countingEmbeddingProvider) Embeddings(context.Context, []string) ([][]float32, error) {
	p.calls++
	return [][]float32{{1, 0, 0}}, nil
}
func (*countingEmbeddingProvider) Enrich(context.Context, string) (Insight, error) {
	return Insight{}, nil
}
func (*countingEmbeddingProvider) ExtractImage(context.Context, string, []byte) (string, error) {
	return "", nil
}
func (*countingEmbeddingProvider) Answer(context.Context, string, []Evidence) (AnswerResult, error) {
	return AnswerResult{}, nil
}

type memoryEmbeddingCache struct{ values map[string][]float32 }

func (c *memoryEmbeddingCache) Get(_ context.Context, key string) ([]float32, bool, error) {
	value, ok := c.values[key]
	return append([]float32(nil), value...), ok, nil
}
func (c *memoryEmbeddingCache) Set(_ context.Context, key string, value []float32) error {
	if c.values == nil {
		c.values = map[string][]float32{}
	}
	c.values[key] = append([]float32(nil), value...)
	return nil
}

func TestEmbedQueryUsesVersionedCacheAndProviderFallback(t *testing.T) {
	provider := &countingEmbeddingProvider{}
	cache := &memoryEmbeddingCache{}
	service := &Service{provider: provider, embeddingCache: cache, config: config.AI{EmbeddingModel: "model-v1", Dimension: 3}}
	first, err := service.embedQuery(context.Background(), "hello world")
	if err != nil || len(first) != 1 || provider.calls != 1 {
		t.Fatalf("first query = %#v err=%v provider calls=%d", first, err, provider.calls)
	}
	second, err := service.embedQuery(context.Background(), "hello world")
	if err != nil || len(second) != 1 || provider.calls != 1 {
		t.Fatalf("cached query = %#v err=%v provider calls=%d", second, err, provider.calls)
	}
	if _, err := service.embedQuery(context.Background(), "different"); err != nil || provider.calls != 2 {
		t.Fatalf("cache miss did not fall back: err=%v provider calls=%d", err, provider.calls)
	}
}
