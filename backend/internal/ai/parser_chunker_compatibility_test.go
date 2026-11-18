package ai

import (
	"context"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
)

func TestParserLeavesFrozenAndCurrentParentChildSplittingToContentWorker(t *testing.T) {
	for _, version := range []string{"parent-child-v1", "parent-child-v2", "parent-child-v3", rag.ParentChildVersion} {
		t.Run(version, func(t *testing.T) {
			text := strings.Repeat("中文原文预算", 500)
			inner := &parserStub{result: ParseResult{Sections: []Section{{Text: text}}}}
			parser := NewChunkingParser(inner, rag.ChunkOptions{TokenBudget: 100, MaxChunks: 1})
			out, err := parser.Parse(context.Background(), ParseRequest{ChunkerVersion: version})
			if err != nil || inner.calls != 1 || len(out.Sections) != 1 || out.Sections[0].Text != text || len(out.Chunks) != 0 || out.ChunksTruncated {
				t.Fatal("parent-child source was changed or flattened by parser", out, err)
			}
		})
	}
}
