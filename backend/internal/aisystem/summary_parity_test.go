package aisystem

import (
	"context"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
)

type summaryProbe struct {
	ai.Provider
	calls int
	t     *testing.T
}

func (p *summaryProbe) Enrich(_ context.Context, text string) (ai.Insight, error) {
	p.calls++
	if rag.EstimateTokens(text) > 6000 {
		p.t.Fatal("oversized summary request")
	}
	tail := ""
	if strings.Contains(text, "TAIL937") {
		tail = "TAIL937"
	}
	return ai.Insight{Summary: strings.Repeat("概", 490) + tail}, nil
}

func TestSummaryReducesAllPartsWithinSharedBudget(t *testing.T) {
	p := &summaryProbe{t: t}
	c := &Computer{Summary: p, SummaryCalls: 30}
	parts := make([]string, 20)
	for i := range parts {
		parts[i] = "第一层内容"
	}
	parts[19] += "TAIL937"
	insight, leaves, e := c.summarize(context.Background(), parts)
	if e != nil || len(leaves) != 20 || p.calls != 23 || !strings.Contains(insight.Summary, "TAIL937") {
		t.Fatal("hierarchical tail coverage failed", e, p.calls)
	}
	p.calls = 0
	c.SummaryCalls = 2
	_, _, e = c.summarize(context.Background(), parts)
	if e == nil || !IsPermanentComputeError(e) || p.calls != 0 {
		t.Fatal("call budget did not converge", e, p.calls)
	}
	p.calls = 0
	c.SummaryCalls = 2
	_, _, e = c.summarize(context.Background(), []string{"one"})
	if e != nil || p.calls != 1 {
		t.Fatal("single part wasted reduction call", e, p.calls)
	}
}
