package rag

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMakeChunksKeepsHeadingAndSentenceBoundaries(t *testing.T) {
	chunks := MakeChunks([]Section{{Text: "## 检索链路\n\n第一段说明全文检索和向量检索。\n\n第二段说明 RRF 融合。"}})
	if len(chunks) != 1 {
		t.Fatalf("expected one compact chunk, got %d", len(chunks))
	}
	if !strings.HasPrefix(chunks[0].Content, "## 检索链路") || !strings.HasSuffix(chunks[0].Content, "融合。") {
		t.Fatalf("heading or paragraph boundary was lost: %q", chunks[0].Content)
	}
}

func TestMakeChunksHardSplitsOversizedSentence(t *testing.T) {
	chunks := MakeChunks([]Section{{Text: strings.Repeat("超长句", ChunkSize)}})
	if len(chunks) <= 1 {
		t.Fatal("expected oversized sentence to be split")
	}
	for _, chunk := range chunks {
		if size := utf8.RuneCountInString(chunk.Content); size > ChunkSize {
			t.Fatalf("chunk has %d runes, limit is %d", size, ChunkSize)
		}
	}
}
