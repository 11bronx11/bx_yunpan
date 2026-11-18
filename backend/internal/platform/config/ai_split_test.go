package config

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// A09/E06: all 16 flag configurations and minimal role requirements. These are
// configuration contracts; runtime route coverage is recorded separately.
func TestCapabilityFlagMatrixAndRoleModelRequirements(t *testing.T) {
	for mask := range 16 {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			for n, key := range []string{"AI_ENABLED", "AI_ASK_ENABLED", "AI_VECTORS_ENABLED", "AI_SUMMARY_ENABLED"} {
				t.Setenv(key, strconv.FormatBool(mask&(1<<n) != 0))
			}
			a := loadAISplit()
			if a.Ask != (mask&2 != 0) || a.Vectors != (mask&4 != 0) || a.Summary != (mask&8 != 0) {
				t.Fatal("capabilities coupled")
			}
			a.CoreToken = "fixture"
			a.ScopePublicKey = "fixture"
			a.ProcessingPublicKey = "fixture"
			a.QueryToken = "fixture"
			a.QueryDSN = "postgres://fixture/ai"
			a.ComputeDSN = a.QueryDSN
			a.CommitDSN = a.QueryDSN
			cfg := Config{Role: "ai-query", AISplit: a, AI: AI{Provider: "dashscope", Dimension: 1024, RequestTimeout: time.Second, MaxObjectBytes: 1024, RerankCandidates: 30, RerankTopK: 8, RerankTimeout: time.Second}, Search: Search{ReadBackend: "postgres", ExternalTimeout: time.Second}}
			err := cfg.validateAISplit()
			if (err != nil) != (a.Ask || a.Vectors) {
				t.Fatal("query model requirement does not follow enabled capabilities", err)
			}
			cfg.Role = "ai-content-worker"
			cfg.Async.ControlDSN = "postgres://fixture/control"
			cfg.TaskQueue.URL = "amqp://fixture"
			if err := cfg.validateAISplit(); err != nil {
				t.Fatal("text preparation requires model key", err)
			}
			cfg.AISplit.QueryDSN = ""
			cfg.Role = "ai-query"
			if err := cfg.validateAISplit(); err == nil {
				t.Fatal("missing explicit AI DSN accepted")
			}
		})
	}
}

func TestSplitQueryRejectsInvalidRuntimeSettings(t *testing.T) {
	a := loadAISplit()
	a.CoreToken, a.QueryToken, a.ScopePublicKey, a.ProcessingPublicKey = "fixture", "fixture", "fixture", "fixture"
	a.QueryDSN = "postgres://fixture/ai"
	base := Config{Role: "ai-query", AISplit: a, AI: AI{Provider: "fake", Dimension: 1024, RequestTimeout: time.Second, RerankCandidates: 30, RerankTopK: 8, RerankTimeout: time.Second}, Search: Search{ReadBackend: "postgres", ExternalTimeout: time.Second}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		set  func(*Config)
	}{
		{"provider-typo", func(c *Config) { c.AI.Provider = "dashscoep" }},
		{"candidate-zero", func(c *Config) { c.AI.RerankCandidates = 0 }},
		{"top-k-over-candidates", func(c *Config) { c.AI.RerankTopK = 31 }},
		{"rerank-timeout-zero", func(c *Config) { c.AI.RerankTimeout = 0 }},
		{"enabled-reranker-without-model", func(c *Config) { c.AI.RerankEnabled = true }},
		{"model-timeout-zero", func(c *Config) { c.AI.RequestTimeout = 0 }},
		{"query-timeout-zero", func(c *Config) { c.AISplit.QueryTimeout = 0 }},
		{"search-backend-typo", func(c *Config) { c.Search.ReadBackend = "es-qdrants" }},
		{"external-timeout-zero", func(c *Config) { c.Search.ExternalTimeout = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.set(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid split role configuration accepted")
			}
		})
	}
}
