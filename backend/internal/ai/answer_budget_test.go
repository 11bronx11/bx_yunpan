package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func TestAnswerOutputBudgetReachesProviderAndRespectsModelLimit(t *testing.T) {
	var gotTokens int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		gotTokens = payload.MaxTokens
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"answer\":\"fixture\",\"grounded\":true,\"citations\":[\"c1\"]}"}}]}`)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name                   string
		requested, model, want int
	}{
		{"unselected", 0, 32768, 4096},
		{"unselected_unchanged_by_model_ceiling", 0, 2048, 4096},
		{"one", 20480, 32768, 20480},
		{"two_to_three", 16384, 32768, 16384},
		{"four_to_five", 12288, 32768, 12288},
		{"six_to_nine", 8192, 32768, 8192},
		{"ten_or_more", 4096, 32768, 4096},
		{"model_below_single_tier", 20480, 8192, 8192},
		{"model_below_all_tiers", 4096, 2048, 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := NewProvider(config.AI{Provider: "dashscope", BaseURL: server.URL, ChatModel: "test-model", ChatMaxOutputTokens: tc.model, RequestTimeout: time.Second})
			answer, err := provider.Answer(context.Background(), "question", []Evidence{{ID: "c1", Content: "fixture"}}, AnswerOptions{MaxTokens: tc.requested})
			if err != nil || !answer.Grounded || gotTokens != tc.want {
				t.Fatalf("answer=%+v error=%v max_tokens=%d, want %d", answer, err, gotTokens, tc.want)
			}
		})
	}
}

type answerBudgetTransport func(*http.Request) (*http.Response, error)

func (f answerBudgetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSelectedAnswerTimeoutDoesNotChangeSharedClient(t *testing.T) {
	var remaining time.Duration
	client := &http.Client{Timeout: time.Minute, Transport: answerBudgetTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("provider request has no deadline")
		}
		remaining = time.Until(deadline)
		body := `{"choices":[{"message":{"content":"{\"answer\":\"fixture\",\"grounded\":false,\"citations\":[]}"}}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	provider := &dashScopeProvider{baseURL: "http://provider.test", client: client, maxOutputTokens: 32768}
	for _, multiplier := range []int{5, 4, 3, 2, 1} {
		t.Run(fmt.Sprint(multiplier), func(t *testing.T) {
			_, err := provider.Answer(context.Background(), "question", nil, AnswerOptions{MaxTokens: 4096 * multiplier, TimeoutMultiplier: multiplier})
			want := time.Duration(multiplier) * time.Minute
			if err != nil || remaining > want || remaining < want-time.Second || client.Timeout != time.Minute {
				t.Fatalf("error=%v remaining=%s want=%s shared timeout=%s", err, remaining, want, client.Timeout)
			}
		})
	}
}
