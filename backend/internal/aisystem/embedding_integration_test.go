package aisystem

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func TestEmbeddingComputeHonorsProviderLimitAndPublishesEveryChild(t *testing.T) {
	db, store, publisher, in, _ := readyText(t)
	var children int64
	if err := db.Table("ai_published_children").Where("content_version_id=?", in.ContentVersionID).Count(&children).Error; err != nil {
		t.Fatal(err)
	}
	if children <= 20 {
		t.Fatal("fixture must exceed the real provider's batch limit", children)
	}
	fake := ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})
	var calls, received int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Input []string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		// Enforce the older ten-item limit as well, so this catches regressions
		// across all supported models, including this incident's twenty-item one.
		if len(request.Input) > 10 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"InvalidParameter","message":"batch too large"}}`))
			return
		}
		received += len(request.Input)
		vectors, _ := fake.Embeddings(r.Context(), request.Input)
		items := make([]map[string]any, len(vectors))
		for n, vector := range vectors {
			items[n] = map[string]any{"index": n, "embedding": vector}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer server.Close()
	computer := &Computer{DB: db, Profile: in.Profile, Embedding: ai.NewProvider(config.AI{Provider: "dashscope", BaseURL: server.URL, Dimension: 1024})}
	computePublish(t, publisher, computer, claim(t, store, "ai.embedding.generate", in))
	var published int64
	if err := db.Table("ai_published_vectors").Where("content_version_id=?", in.ContentVersionID).Count(&published).Error; err != nil {
		t.Fatal(err)
	}
	if published != children || int64(received) != children || calls < 3 {
		t.Fatalf("incomplete batched publication: children=%d received=%d published=%d calls=%d", children, received, published, calls)
	}
}

func TestEmbeddingComputeRequestFailuresUseCorrectRetryBudget(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			db, store, _, in, _ := readyText(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":{"code":"ProviderFixture","message":"private details"}}`))
			}))
			defer server.Close()
			computer := &Computer{DB: db, Profile: in.Profile, Embedding: ai.NewProvider(config.AI{Provider: "dashscope", BaseURL: server.URL, Dimension: 1024})}
			task := claim(t, store, "ai.embedding.generate", in)
			_, err := computer.Compute(context.Background(), task)
			if err == nil {
				t.Fatal("fixture did not reject the embedding request")
			}
			if failErr := store.Fail(context.Background(), task, err, IsPermanentComputeError(err)); failErr != nil {
				t.Fatal(failErr)
			}
			var saved asynccontrol.Task
			if err := store.DB.First(&saved, "id=?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			want, message := asynccontrol.Stopped, "embedding.rejected"
			if code == http.StatusTooManyRequests {
				want, message = asynccontrol.Pending, "embedding.unavailable"
			}
			if saved.State != want || saved.Attempts != 1 || saved.LastError != message {
				t.Fatalf("wrong retry disposition: state=%s attempts=%d error=%s", saved.State, saved.Attempts, saved.LastError)
			}
		})
	}
}
