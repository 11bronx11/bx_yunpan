package ragtest

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
)

// TestReport is intentionally small and deterministic. It is a comparison
// harness for the RAG changes, not a production load test or relevance claim.
func TestReport(t *testing.T) {
	cases := map[string][]rag.Section{
		"markdown":                 {{Text: markdownFixture}},
		"pdf-docx-paragraphs":      {{Text: paragraphFixture, PageNumber: intPtr(2), Section: "document body"}},
		"long-structured-document": {{Text: longFixture()}},
	}
	for name, sections := range cases {
		chunks := rag.MakeChunks(sections)
		lengths := make([]int, 0, len(chunks))
		natural := 0
		for _, chunk := range chunks {
			length := utf8.RuneCountInString(chunk.Content)
			lengths = append(lengths, length)
			if endsAtNaturalBoundary(chunk.Content) {
				natural++
			}
		}
		if len(chunks) == 0 {
			t.Fatalf("%s produced no chunks", name)
		}
		fmt.Printf("RAG_REPORT case=%s chunks=%d avg_runes=%.1f max_runes=%d natural_end=%d/%d (%0.1f%%)\n",
			name, len(chunks), average(lengths), max(lengths), natural, len(chunks), 100*float64(natural)/float64(len(chunks)))
	}

	for _, limit := range []int{20, 40} {
		fmt.Printf("RAG_REPORT search_limit=%d recall_per_leg=%d final_fusion_limit=%d\n", limit, recallLimit(limit), fusionLimit(limit))
	}
}

func BenchmarkChunkingMarkdown(b *testing.B) {
	sections := []rag.Section{{Text: markdownFixture}}
	for index := 0; index < b.N; index++ {
		_ = rag.MakeChunks(sections)
	}
}

func BenchmarkChunkingParagraphs(b *testing.B) {
	sections := []rag.Section{{Text: paragraphFixture}}
	for index := 0; index < b.N; index++ {
		_ = rag.MakeChunks(sections)
	}
}

func BenchmarkChunkingLongDocument(b *testing.B) {
	sections := []rag.Section{{Text: longFixture()}}
	for index := 0; index < b.N; index++ {
		_ = rag.MakeChunks(sections)
	}
}

func endsAtNaturalBoundary(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(value)
	return strings.ContainsRune("。！？!?；;:：.!?", last)
}

func recallLimit(limit int) int {
	if limit > 50 {
		return limit
	}
	return 50
}

func fusionLimit(limit int) int {
	if limit > 30 {
		return limit
	}
	return 30
}

func average(values []int) float64 {
	total := 0
	for _, value := range values {
		total += value
	}
	return float64(total) / float64(len(values))
}

func max(values []int) int {
	result := values[0]
	for _, value := range values[1:] {
		if value > result {
			result = value
		}
	}
	return result
}

func intPtr(value int) *int { return &value }

const markdownFixture = `# 项目交付说明

## 检索链路

系统先从对象存储读取文档，再按文档结构提取文本。检索阶段同时执行 PostgreSQL 全文检索和 pgvector 向量检索，最后使用 RRF 融合两路排名。

## 发布检查

发布前需要完成接口联调、权限验证和回滚演练。每项检查都应记录结果，并保留可以复核的证据。`

const paragraphFixture = `第一段介绍文档的目标和适用范围。这里的内容用于验证段落边界是否被保留。

第二段描述数据处理流程。上传完成后，系统会生成索引任务，抽取文本并写入向量数据库。

第三段说明失败恢复策略。处理租约过期后可以重新获取任务，配额错误则不会无限重试。`

func longFixture() string {
	paragraphs := make([]string, 0, 30)
	for index := 1; index <= 30; index++ {
		paragraphs = append(paragraphs, fmt.Sprintf("## 第 %02d 节 检索实现\n\n系统先读取第 %02d 节的内容并进行权限过滤。随后执行全文检索和向量检索，再将两路排名融合。该节还记录失败重试、引用回验和观测指标，便于复核线上行为。", index, index))
	}
	return strings.Join(paragraphs, "\n\n")
}
