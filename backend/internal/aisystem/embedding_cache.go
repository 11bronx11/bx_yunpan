package aisystem

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
)

func embeddingText(chunker, heading, content string) string {
	if chunker == "parent-child-v3" || chunker == "parent-child-v4" {
		return rag.ContextualText(heading, content, 600)
	}
	// Accepted/frozen v1 and v2 tasks keep their original model input and hash;
	// v3 retains the heading context it already used before the v4 upgrade.
	return content
}

func validVector(vector []float32, dimension int) bool {
	if len(vector) != dimension {
		return false
	}
	var norm float64
	for _, v := range vector {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return false
		}
		norm += float64(v) * float64(v)
	}
	return norm > 0
}

func normalizedVector(vector []float32) []float32 {
	var sum float64
	for _, v := range vector {
		sum += float64(v) * float64(v)
	}
	out := make([]float32, len(vector))
	scale := 1 / math.Sqrt(sum)
	for n, v := range vector {
		out[n] = float32(float64(v) * scale)
	}
	return out
}

func (q *Query) embedQuery(ctx context.Context, text string) ([][]float32, error) {
	// No principal or scope data: this cache holds only derived query vectors.
	// Include provider and revision so a model switch cannot reuse old geometry.
	key := "ai:v2:query-embedding:" + aiprotocol.HashJSON([]any{q.Profile.EmbeddingProvider, q.Profile.EmbeddingModel, q.Profile.EmbeddingRevision, q.Profile.Dimension, "l2", text})
	if q.EmbeddingCache != nil {
		cached, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		vector, ok, err := q.EmbeddingCache.Get(cached, key)
		cancel()
		if err == nil && ok && validVector(vector, 1024) {
			return [][]float32{vector}, nil
		}
	}
	vs, err := q.Provider.Embeddings(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vs) != 1 || !validVector(vs[0], 1024) {
		return nil, errors.New("embedding.invalid_response")
	}
	vs[0] = normalizedVector(vs[0])
	if q.EmbeddingCache != nil {
		cached, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		_ = q.EmbeddingCache.Set(cached, key, vs[0])
		cancel()
	}
	return vs, nil
}
