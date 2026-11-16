package config

import (
	"fmt"
	"strconv"
	"testing"
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
			cfg := Config{Role: "ai-query", AISplit: a, AI: AI{Provider: "dashscope", Dimension: 1024}}
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
