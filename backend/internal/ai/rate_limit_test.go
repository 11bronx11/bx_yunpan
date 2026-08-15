package ai

import "testing"

func TestRateLimiterUsesConfiguredLimits(t *testing.T) {
	limiter := &redisRequestLimiter{limits: RateLimits{
		SearchPerMinute:    60,
		AskPerMinute:       20,
		ReprocessPerMinute: 6,
	}}

	tests := map[string]int{
		"search":    60,
		"ask":       20,
		"reprocess": 6,
		"unknown":   0,
	}
	for scope, expected := range tests {
		if actual := limiter.limit(scope); actual != expected {
			t.Fatalf("scope %q: expected %d, got %d", scope, expected, actual)
		}
	}
}
