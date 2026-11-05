package rag

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChineseHardSplitPreservesSearchableWord(t *testing.T) {
	result := MakeTypedChunks([]Section{{Text: "中华人民共和国", Kind: KindText}}, ChunkOptions{TokenBudget: 5, Overlap: 1})
	found := false
	for _, chunk := range result.Chunks {
		if !strings.Contains("中华人民共和国", chunk.Content) {
			t.Fatalf("chunker inserted text into a source word: %q", chunk.Content)
		}
		found = found || strings.Contains(chunk.Content, "共和国")
	}
	if !found {
		t.Fatalf("word at the split boundary disappeared: %#v", result.Chunks)
	}
}

func TestTypedChunksAreContiguousSourceSlices(t *testing.T) {
	for _, source := range []string{
		strings.Repeat("中文解析，全文检索：索引一致、原文保真；", 20),
		strings.Repeat("第一段保留。\n\n第二段保留。\n第三段保留。", 15),
		strings.Repeat("PostgreSQL 中文搜索\tElasticsearch storage adapter\n", 20),
		strings.Repeat("没有标点的中文内容必须持续前进保持原文不能插入空格", 20),
		strings.Repeat("verylongidentifier", 300),
	} {
		for _, overlap := range []int{0, 1, 5, 63} {
			result := MakeTypedChunks([]Section{{Text: source, Kind: KindText}}, ChunkOptions{TokenBudget: 64, Overlap: overlap})
			if len(result.Chunks) < 2 || result.Truncated && overlap != 63 {
				t.Fatalf("unexpected chunk count/truncation: %d, %v", len(result.Chunks), result.Truncated)
			}
			for _, chunk := range result.Chunks {
				if !utf8.ValidString(chunk.Content) || !strings.Contains(source, chunk.Content) || chunk.TokenCount > 64 {
					t.Fatalf("overlap=%d: invalid source slice or token budget: %#v", overlap, chunk)
				}
			}
		}
	}
}

func TestChineseBigramsSurviveChunkBoundaries(t *testing.T) {
	const source = "中文原文解析支持中华人民共和国和数据库检索以及索引回退保持一致"
	for _, budget := range []int{3, 5, 9} {
		result := MakeTypedChunks([]Section{{Text: source}}, ChunkOptions{TokenBudget: budget, Overlap: 1})
		runes := []rune(source)
		for i := 0; i+1 < len(runes); i++ {
			pair := string(runes[i : i+2])
			found := false
			for _, chunk := range result.Chunks {
				found = found || strings.Contains(chunk.Content, pair)
			}
			if !found {
				t.Fatalf("budget=%d: missing cross-boundary bigram %q", budget, pair)
			}
		}
	}
}

func TestExplicitZeroOverlapIsRespected(t *testing.T) {
	const source = "甲乙丙丁戊己庚辛壬癸"
	result := MakeTypedChunks([]Section{{Text: source}}, ChunkOptions{TokenBudget: 4, Overlap: 0})
	var pieces []string
	for _, chunk := range result.Chunks {
		pieces = append(pieces, chunk.Content)
	}
	if strings.Join(pieces, "") != source {
		t.Fatalf("explicit zero overlap repeated or dropped source: %q", pieces)
	}
}
