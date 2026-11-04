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

func TestSplitTextPreservesWhitespaceBetweenAtoms(t *testing.T) {
	pieces := splitText("hello world hello world", 12, textSeparators)
	if len(pieces) < 2 {
		t.Fatalf("expected text to split, got %#v", pieces)
	}
	for _, piece := range pieces {
		if strings.Contains(piece, "helloworld") {
			t.Fatalf("word boundary was lost: %#v", pieces)
		}
	}
}

func TestMakeChunksFlushesAtSectionBoundaries(t *testing.T) {
	firstPage, secondPage := 2, 3
	chunks := MakeChunks([]Section{
		{Text: "第一页的末段。", PageNumber: &firstPage, Section: "第一章"},
		{Text: "第二页的开头。", PageNumber: &secondPage, Section: "第二章"},
	})
	if len(chunks) != 2 {
		t.Fatalf("expected one chunk per section, got %d", len(chunks))
	}
	if chunks[0].PageNumber == nil || *chunks[0].PageNumber != firstPage || chunks[0].Section != "第一章" {
		t.Fatalf("first chunk lost source metadata: %#v", chunks[0])
	}
	if chunks[1].PageNumber == nil || *chunks[1].PageNumber != secondPage || chunks[1].Section != "第二章" {
		t.Fatalf("second chunk lost source metadata: %#v", chunks[1])
	}
	if strings.Contains(chunks[0].Content, chunks[1].Content) || strings.Contains(chunks[1].Content, chunks[0].Content) {
		t.Fatalf("section content crossed a source boundary: %#v", chunks)
	}
}

func TestMakeChunksRepeatsHeadingContext(t *testing.T) {
	text := "# 运维指南\n\n" + strings.Repeat("部署步骤需要逐项核对配置和运行状态。 ", 100)
	chunks := MakeChunks([]Section{{Text: text}})
	if len(chunks) < 2 {
		t.Fatalf("expected heading section to span multiple chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if !strings.HasPrefix(chunk.Content, "# 运维指南\n\n") {
			t.Fatalf("chunk is missing heading context: %q", chunk.Content[:minInt(40, len(chunk.Content))])
		}
		if runeCount(chunk.Content) > ChunkSize {
			t.Fatalf("chunk has %d runes, limit is %d", runeCount(chunk.Content), ChunkSize)
		}
	}
}

func TestMakeChunksOverlapsNeighboringPieces(t *testing.T) {
	text := strings.Repeat("token ", ChunkSize*3)
	chunks := MakeChunks([]Section{{Text: text}})
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	overlap := overlapTail(chunks[0].Content, ChunkOverlap)
	if overlap == "" || !strings.Contains(chunks[1].Content, overlap) {
		t.Fatalf("neighboring chunks have no shared context: overlap=%q second=%q", overlap, chunks[1].Content[:minInt(80, len(chunks[1].Content))])
	}
}

func TestMakeChunksRepeatsTableHeader(t *testing.T) {
	rows := make([]string, 0, 40)
	for index := 0; index < 40; index++ {
		rows = append(rows, "| row-"+strings.Repeat("x", 35)+" | value-"+strings.Repeat("y", 20)+" |")
	}
	text := "| name | value |\n| --- | --- |\n" + strings.Join(rows, "\n")
	chunks := MakeChunks([]Section{{Text: text, Kind: KindTable}})
	if len(chunks) < 2 {
		t.Fatalf("expected table to be split into multiple chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if !strings.Contains(chunk.Content, "| name | value |\n| --- | --- |") {
			t.Fatalf("table chunk is missing its header: %q", chunk.Content)
		}
		if runeCount(chunk.Content) > ChunkSize {
			t.Fatalf("table chunk has %d runes, limit is %d", runeCount(chunk.Content), ChunkSize)
		}
	}
}

func TestMakeTypedChunksCarriesTableSourceMetadata(t *testing.T) {
	result := MakeTypedChunks([]Section{{
		Text: "| name | value |\n| --- | --- |\n| alpha | 3 |", Kind: KindTable,
		SheetName: "North", RowStart: 2, RowEnd: 2, ColumnStart: 1, ColumnEnd: 2,
		HasHeader: true, MergedRanges: []string{"A1:B1"}, Encoding: "utf-8",
	}}, ChunkOptions{TokenBudget: 128, MaxChunks: 4})
	if len(result.Chunks) != 1 {
		t.Fatalf("expected one table chunk, got %d", len(result.Chunks))
	}
	if result.Version != DefaultChunker || result.TokenizerID != EstimateTokenizer {
		t.Fatalf("effective chunker identity = %q/%q", result.Version, result.TokenizerID)
	}
	chunk := result.Chunks[0]
	if chunk.SheetName != "North" || chunk.RowStart != 2 || chunk.RowEnd != 2 || chunk.Encoding != "utf-8" || len(chunk.MergedRanges) != 1 || chunk.MergedRanges[0] != "A1:B1" {
		t.Fatalf("table source metadata was lost: %#v", chunk)
	}
}

func TestMakeTypedChunksCoalescesParagraphsWithinSourceBoundary(t *testing.T) {
	page := 3
	result := MakeTypedChunks([]Section{
		{
			Text: "First paragraph.", Kind: KindText, HeadingPath: "Guide",
			PageNumber: &page, PageEnd: 3, SourceStart: 0, SourceEnd: 18,
		},
		{
			Text: "Second paragraph.", Kind: KindText, HeadingPath: "Guide",
			PageNumber: &page, PageEnd: 3, SourceStart: 19, SourceEnd: 37,
		},
	}, ChunkOptions{TokenBudget: 128, Overlap: 0, MaxChunks: 8})
	if len(result.Chunks) != 1 {
		t.Fatalf("expected adjacent paragraphs to share a chunk, got %d", len(result.Chunks))
	}
	chunk := result.Chunks[0]
	if !strings.Contains(chunk.Content, "First paragraph.\n\nSecond paragraph.") || chunk.HeadingPath != "Guide" {
		t.Fatalf("paragraphs or heading context were lost: %#v", chunk)
	}
	if chunk.PageStart != 3 || chunk.PageEnd != 3 || chunk.SourceStart != 0 || chunk.SourceEnd != 37 {
		t.Fatalf("coalesced source range was not preserved: %#v", chunk)
	}
}

func TestMakeTypedChunksKeepsListAndPageBoundaries(t *testing.T) {
	firstPage, secondPage := 1, 2
	result := MakeTypedChunks([]Section{
		{Text: "- first item", Kind: KindList, HeadingPath: "Steps", PageNumber: &firstPage},
		{Text: "- second item", Kind: KindList, HeadingPath: "Steps", PageNumber: &firstPage},
		{Text: "- next page", Kind: KindList, HeadingPath: "Steps", PageNumber: &secondPage},
	}, ChunkOptions{TokenBudget: 128, Overlap: 0, MaxChunks: 8})
	if len(result.Chunks) != 2 {
		t.Fatalf("expected list items to group without crossing pages, got %d chunks", len(result.Chunks))
	}
	if result.Chunks[0].Kind != KindList || !strings.Contains(result.Chunks[0].Content, "- first item\n- second item") {
		t.Fatalf("list items were not grouped as a list: %#v", result.Chunks[0])
	}
	if result.Chunks[1].PageStart != 2 || strings.Contains(result.Chunks[0].Content, "next page") {
		t.Fatalf("list content crossed a page boundary: %#v", result.Chunks)
	}
}

func TestMakeTypedChunksAppliesTokenBudgetAfterGrouping(t *testing.T) {
	page := 4
	result := MakeTypedChunks([]Section{
		{Text: strings.Repeat("检索内容。", 20), Kind: KindText, HeadingPath: "Search", PageNumber: &page},
		{Text: strings.Repeat("继续说明。", 20), Kind: KindText, HeadingPath: "Search", PageNumber: &page},
	}, ChunkOptions{TokenBudget: 32, Overlap: 4, MaxChunks: 32})
	if len(result.Chunks) < 2 {
		t.Fatalf("expected grouped paragraphs to split at the token budget, got %d chunk(s)", len(result.Chunks))
	}
	for _, chunk := range result.Chunks {
		if chunk.TokenCount > 32 || !chunk.TokenCountIsEstimate {
			t.Fatalf("chunk exceeds its declared token estimate budget: %#v", chunk)
		}
		if !strings.Contains(chunk.Content, "Search") {
			t.Fatalf("split chunk lost heading context: %q", chunk.Content)
		}
	}
}

func TestMakeChunksRespectsDocumentCap(t *testing.T) {
	chunks := MakeChunks([]Section{{Text: strings.Repeat("x", (MaxChunksPerDocument+20)*ChunkSize)}})
	if len(chunks) != MaxChunksPerDocument {
		t.Fatalf("expected %d chunks, got %d", MaxChunksPerDocument, len(chunks))
	}
	for _, chunk := range chunks {
		if runeCount(chunk.Content) > ChunkSize {
			t.Fatalf("chunk has %d runes, limit is %d", runeCount(chunk.Content), ChunkSize)
		}
	}
}
