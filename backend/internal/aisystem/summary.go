package aisystem

import (
	"context"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
)

// Every level covers the preceding level in full. The shared call budget bounds
// pathological model outputs without dropping the end of the document.
func (c *Computer) summarize(ctx context.Context, parts []string) (ai.Insight, []string, error) {
	calls, budget := 0, max(c.SummaryCalls, 2)
	var insight ai.Insight
	var leaves []string
	for level := 0; ; level++ {
		if len(parts) > budget-calls {
			return insight, leaves, &ai.ParseError{Code: "ai.summary.call_budget_exceeded", Permanent: true}
		}
		summaries := make([]string, 0, len(parts))
		for _, part := range parts {
			var err error
			insight, err = c.Summary.Enrich(ctx, part)
			calls++
			if err != nil {
				return insight, leaves, &modelBuildError{stage: "summary", cause: err}
			}
			if strings.TrimSpace(insight.Summary) == "" {
				return insight, leaves, &ai.ParseError{Code: "ai.summary.empty", Permanent: true}
			}
			summaries = append(summaries, insight.Summary)
		}
		if level == 0 {
			leaves = summaries
		}
		if len(summaries) == 1 {
			return insight, leaves, nil
		}
		parts = rag.SummaryParts([]rag.Section{{Text: strings.Join(summaries, "\n")}}, 6000)
	}
}
