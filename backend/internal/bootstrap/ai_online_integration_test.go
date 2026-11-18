package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Pause an external read only after the query has resolved its core scope.
// Mutations can then commit at a known point while real Search/Ask calls wait.
type onlineProjectionGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newOnlineProjectionGate() *onlineProjectionGate {
	return &onlineProjectionGate{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (g *onlineProjectionGate) open() { g.once.Do(func() { close(g.release) }) }
func (g *onlineProjectionGate) wait(ctx context.Context) bool {
	select {
	case g.entered <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	select {
	case <-g.release:
		return true
	case <-ctx.Done():
		return false
	}
}

type onlineHTTPResult struct {
	Code     int
	Body     map[string]any
	Err      error
	Duration time.Duration
}

func runAIOnlineChecks(t *testing.T, ctx context.Context, base, token, root, a, b string, esFail, qdFail *atomic.Bool, qdGate *atomic.Pointer[onlineProjectionGate], upload func(string, string, ...string) string) {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	call := func(auth, method, path string, body any, headers map[string]string) onlineHTTPResult {
		start := time.Now()
		var reader io.Reader
		if body != nil {
			payload, _ := json.Marshal(body)
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
		if err != nil {
			return onlineHTTPResult{Err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			return onlineHTTPResult{Err: err, Duration: time.Since(start)}
		}
		defer res.Body.Close()
		out := map[string]any{}
		if res.StatusCode != 204 {
			err = json.NewDecoder(res.Body).Decode(&out)
		}
		return onlineHTTPResult{res.StatusCode, out, err, time.Since(start)}
	}
	must := func(t *testing.T, out onlineHTTPResult, want int) map[string]any {
		t.Helper()
		if out.Err != nil || out.Code != want {
			t.Fatalf("HTTP=%d want=%d err=%v body=%v", out.Code, want, out.Err, out.Body)
		}
		return out.Body
	}
	search := func(mode string) map[string]any { return map[string]any{"query": "rollback", "mode": mode} }
	ask := func(ids ...string) map[string]any {
		body := map[string]any{"question": "rollback 回滚策略"}
		if len(ids) > 0 {
			body["scope"] = map[string]any{"mode": "selected", "file_ids": ids}
		}
		return body
	}
	backends := func(t *testing.T, out map[string]any, text, vector string) {
		t.Helper()
		meta := out["retrieval"].(map[string]any)
		if text != "" && meta["text_backend"] != text || vector != "" && meta["vector_backend"] != vector {
			t.Fatalf("wrong retrieval backend: %v want text=%s vector=%s", meta, text, vector)
		}
	}

	t.Run("projection-failure-and-recovery", func(t *testing.T) {
		defer esFail.Store(false)
		defer qdFail.Store(false)
		for _, tc := range []struct {
			name   string
			es, qd bool
		}{{"healthy", false, false}, {"es-down", true, false}, {"qd-down", false, true}, {"both-down", true, true}, {"recovered", false, false}} {
			t.Run(tc.name, func(t *testing.T) {
				esFail.Store(tc.es)
				qdFail.Store(tc.qd)
				text, vector := "elasticsearch", "qdrant"
				if tc.es {
					text = "postgres"
				}
				if tc.qd {
					vector = "postgres"
				}
				for _, mode := range []string{"fulltext", "semantic", "hybrid"} {
					out := must(t, call(token, "POST", "/api/v1/search", search(mode), nil), 200)
					if len(out["hits"].([]any)) == 0 {
						t.Fatal("failure lost usable search evidence", mode)
					}
					wantText, wantVector := text, vector
					if mode == "fulltext" {
						wantVector = ""
					}
					if mode == "semantic" {
						wantText = ""
					}
					backends(t, out, wantText, wantVector)
				}
				for _, body := range []map[string]any{ask(a), ask(a, b), ask()} {
					out := must(t, call(token, "POST", "/api/v1/ai/ask", body, nil), 200)
					if len(out["citations"].([]any)) == 0 || out["grounded"] != true {
						t.Fatal("failure lost grounded Ask", out)
					}
					backends(t, out, text, vector)
					if out["retrieval"].(map[string]any)["degraded"] != (tc.es || tc.qd) {
						t.Fatal("degradation state did not recover", out["retrieval"])
					}
				}
			})
		}
	})
	t.Run("qd-timeout-fallback-and-recovery", func(t *testing.T) {
		gate := newOnlineProjectionGate()
		qdGate.Store(gate)
		defer func() { gate.open(); qdGate.Store(nil) }()
		out := must(t, call(token, "POST", "/api/v1/search", search("semantic"), nil), 200)
		backends(t, out, "", "postgres")
		if len(out["hits"].([]any)) == 0 {
			t.Fatal("timeout lost usable vector results")
		}
		gate.open()
		qdGate.Store(nil)
		backends(t, must(t, call(token, "POST", "/api/v1/search", search("semantic"), nil), 200), "", "qdrant")
	})
	other := must(t, call("", "POST", "/api/v1/auth/register", map[string]any{"username": "outside_" + uuid.NewString()[:8], "password": "online-test-password"}, nil), 201)["access_token"].(string)
	t.Run("authentication-and-owner-isolation", func(t *testing.T) {
		for _, auth := range []string{"", "invalid-token"} {
			must(t, call(auth, "POST", "/api/v1/search", search("fulltext"), nil), 401)
			must(t, call(auth, "POST", "/api/v1/ai/ask", ask(a), nil), 401)
		}
		for _, mode := range []string{"name", "fulltext", "semantic", "hybrid"} {
			if mode == "semantic" {
				// An empty scope has no compatible vectors under the current
				// semantic-readiness contract; it must disclose no foreign data.
				out := must(t, call(other, "POST", "/api/v1/search", search(mode), nil), 503)
				if out["error"].(map[string]any)["code"] != "ai.semantic_unavailable" {
					t.Fatal("empty semantic scope changed its public failure", out)
				}
				continue
			}
			out := must(t, call(other, "POST", "/api/v1/search", search(mode), nil), 200)
			if len(out["hits"].([]any)) != 0 {
				t.Fatal("foreign search evidence returned", mode, out)
			}
		}
		out := must(t, call(other, "POST", "/api/v1/ai/ask", ask(), nil), 200)
		if len(out["citations"].([]any)) != 0 || out["grounded"] != false {
			t.Fatal("foreign all-scope Ask grounded", out)
		}
		must(t, call(other, "POST", "/api/v1/ai/ask", ask(a), nil), 409)
		must(t, call(other, "POST", "/api/v1/ai/ask", map[string]any{"question": "rollback", "scope": map[string]any{"mode": "folder", "folder_id": root}}, nil), 409)
		must(t, call(other, "GET", "/api/v1/files/"+a+"/ai", nil, nil), 404)
		must(t, call(other, "POST", "/api/v1/files/"+a+"/ai/reprocess", map[string]any{"targets": []string{"summary"}}, map[string]string{"Idempotency-Key": uuid.NewString()}), 404)
		for _, method := range []string{"PATCH", "DELETE"} {
			must(t, call(other, method, "/api/v1/files/"+a, map[string]any{"name": "intrusion.txt"}, map[string]string{"If-Match": "1"}), 404)
		}
		must(t, call(token, "POST", "/api/v1/ai/ask", ask(a, uuid.NewString()), nil), 409)
	})
	t.Run("empty-semantic-scope-cannot-trip-other-users", func(t *testing.T) {
		for range 25 {
			out := must(t, call(other, "POST", "/api/v1/search", search("semantic"), nil), 503)
			if out["error"].(map[string]any)["code"] != "ai.semantic_unavailable" {
				t.Fatal("scope readiness opened a shared service circuit", out)
			}
		}
		backends(t, must(t, call(token, "POST", "/api/v1/search", search("semantic"), nil), 200), "", "qdrant")
	})
	for _, concurrency := range []int{1, 4, 8} {
		t.Run(fmt.Sprintf("mixed-concurrency-%d", concurrency), func(t *testing.T) {
			jobs := make(chan int, 24)
			results := make(chan onlineHTTPResult, 24)
			var wg sync.WaitGroup
			for range concurrency {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for n := range jobs {
						if n%4 == 3 {
							results <- call(token, "POST", "/api/v1/ai/ask", ask(a, b), nil)
						} else {
							results <- call(token, "POST", "/api/v1/search", search([]string{"fulltext", "semantic", "hybrid"}[n%4]), nil)
						}
					}
				}()
			}
			for n := range 24 {
				jobs <- n
			}
			close(jobs)
			wg.Wait()
			close(results)
			var times []time.Duration
			for result := range results {
				out := must(t, result, 200)
				times = append(times, result.Duration)
				if hits, ok := out["hits"]; ok {
					if len(hits.([]any)) == 0 {
						t.Fatal("concurrent search lost evidence")
					}
				} else {
					cites := out["citations"].([]any)
					if len(cites) == 0 {
						t.Fatal("concurrent Ask lost evidence")
					}
					for _, raw := range cites {
						id := raw.(map[string]any)["file_id"]
						if id != a && id != b {
							t.Fatal("concurrent Ask escaped selected scope", id)
						}
					}
				}
			}
			sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
			t.Logf("bounded mixed requests=24 concurrency=%d p50=%s p95=%s", concurrency, times[len(times)/2], times[22])
		})
	}

	for _, mutation := range []string{"rename", "move", "delete"} {
		t.Run("inflight-"+mutation, func(t *testing.T) {
			file := must(t, call(token, "GET", "/api/v1/files/"+a, nil, nil), 200)
			version := strconv.FormatInt(int64(file["version"].(float64)), 10)
			gate := newOnlineProjectionGate()
			qdGate.Store(gate)
			defer func() { gate.open(); qdGate.Store(nil) }()
			results := make(chan onlineHTTPResult, 6)
			for n := range 6 {
				go func(n int) {
					if n%2 == 0 {
						results <- call(token, "POST", "/api/v1/search", search("semantic"), nil)
					} else {
						results <- call(token, "POST", "/api/v1/ai/ask", ask(a), nil)
					}
				}(n)
			}
			for range 6 {
				select {
				case <-gate.entered:
				case <-time.After(1500 * time.Millisecond):
					t.Fatal("concurrent requests did not reach the read barrier")
				}
			}
			switch mutation {
			case "rename":
				must(t, call(token, "PATCH", "/api/v1/files/"+a, map[string]any{"name": "online-renamed.txt"}, map[string]string{"If-Match": version}), 200)
			case "move":
				folder := must(t, call(token, "POST", "/api/v1/folders", map[string]any{"parent_id": root, "name": "online-moved"}, nil), 201)["id"].(string)
				must(t, call(token, "POST", "/api/v1/files/"+a+"/move", map[string]any{"target_folder_id": folder}, map[string]string{"If-Match": version}), 200)
			case "delete":
				must(t, call(token, "DELETE", "/api/v1/files/"+a, nil, map[string]string{"If-Match": version}), 204)
			}
			// An unauthorized user racing the same mutations never acquires scope.
			must(t, call(other, "POST", "/api/v1/ai/ask", ask(a), nil), 409)
			gate.open()
			qdGate.Store(nil)
			for range 6 {
				out := must(t, <-results, 409)
				if out["error"].(map[string]any)["code"] != "ai.scope_changed" {
					t.Fatal("in-flight mutation was not fenced", out)
				}
			}
			if mutation == "delete" {
				must(t, call(token, "POST", "/api/v1/ai/ask", ask(a), nil), 409)
				out := must(t, call(token, "POST", "/api/v1/search", search("semantic"), nil), 200)
				for _, raw := range out["hits"].([]any) {
					if raw.(map[string]any)["file"].(map[string]any)["id"] == a {
						t.Fatal("deleted file remained searchable")
					}
				}
			} else {
				out := must(t, call(token, "POST", "/api/v1/ai/ask", ask(a), nil), 200)
				for _, raw := range out["citations"].([]any) {
					c := raw.(map[string]any)
					if c["file_id"] != a || c["file_name"] != "online-renamed.txt" {
						t.Fatal("fresh Ask used stale metadata", c)
					}
				}
				if mutation == "move" {
					body := search("semantic")
					body["folder_id"], body["include_subfolders"] = root, false
					out = must(t, call(token, "POST", "/api/v1/search", body, nil), 200)
					for _, raw := range out["hits"].([]any) {
						if raw.(map[string]any)["file"].(map[string]any)["id"] == a {
							t.Fatal("moved file escaped its new folder")
						}
					}
				}
			}
		})
	}
	t.Run("replacement-content-and-identity", func(t *testing.T) {
		// Logical files point to immutable content-addressed objects. Content
		// replacement uses delete/upload; there is no in-place edit endpoint.
		replacement := upload("online-renamed.txt", "rollback replacementmarker 新文件使用全新内容，旧文件不可再引用。")
		until := time.Now().Add(60 * time.Second)
		for time.Now().Before(until) {
			out := must(t, call(token, "GET", "/api/v1/files/"+replacement+"/ai", nil, nil), 200)
			caps := out["capabilities"].(map[string]any)
			if caps["askable"] == true && caps["vector"].(map[string]any)["available"] == true {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		body := search("fulltext")
		body["query"] = "replacementmarker"
		out := must(t, call(token, "POST", "/api/v1/search", body, nil), 200)
		if len(out["hits"].([]any)) != 1 || out["hits"].([]any)[0].(map[string]any)["file"].(map[string]any)["id"] != replacement {
			t.Fatal("replacement content not found under its new identity", out)
		}
		must(t, call(token, "POST", "/api/v1/ai/ask", ask(a), nil), 409)
		out = must(t, call(token, "POST", "/api/v1/ai/ask", ask(replacement), nil), 200)
		if len(out["citations"].([]any)) == 0 {
			t.Fatal("replacement Ask not grounded")
		}
		for _, raw := range out["citations"].([]any) {
			if raw.(map[string]any)["file_id"] != replacement {
				t.Fatal("replacement cited old identity", raw)
			}
		}
	})
}
