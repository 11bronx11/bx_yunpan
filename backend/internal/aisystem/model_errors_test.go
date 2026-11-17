package aisystem

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func TestModelBuildErrorRetainsRetryClassificationWithoutResponseDetails(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 408, 409, 429, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":{"code":"PrivateProviderCode","message":"private provider response"}}`))
			}))
			defer server.Close()
			provider := ai.NewProvider(config.AI{Provider: "dashscope", BaseURL: server.URL, Dimension: 1024})
			_, embeddingErr := provider.Embeddings(context.Background(), []string{"fixture"})
			_, summaryErr := provider.Enrich(context.Background(), "fixture")
			permanent := code >= 400 && code < 500 && code != 408 && code != 409 && code != 429
			for stage, cause := range map[string]error{"embedding": embeddingErr, "summary": summaryErr} {
				if cause == nil {
					t.Fatal("fixture did not reject request")
				}
				err := &modelBuildError{stage: stage, cause: cause}
				if IsPermanentComputeError(err) != permanent {
					t.Fatalf("%s HTTP %d: permanent=%t, want %t", stage, code, IsPermanentComputeError(err), permanent)
				}
				want := stage + ".unavailable"
				if permanent {
					want = stage + ".rejected"
				}
				if err.Error() != want || strings.Contains(err.Error(), "PrivateProviderCode") {
					t.Fatalf("unsafe or incorrect task error: %v", err)
				}
			}
		})
	}
}
