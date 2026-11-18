package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/textsearch"
	"github.com/google/uuid"
)

func lexicalChildContains(children []Child, query string) bool {
	terms := strings.Fields(textsearch.QueryText(query))
	for _, child := range children {
		index := " " + textsearch.IndexText(child.Content) + " "
		matched := len(terms) > 0
		for _, term := range terms {
			matched = matched && strings.Contains(index, " "+term+" ")
		}
		if matched && strings.Contains(textsearch.Normalize(child.Content), textsearch.Normalize(query)) {
			return true
		}
	}
	return false
}

func TestParentChildBoundaryTermsRemainSearchable(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, query string
	}{
		{"child", strings.Repeat("甲", 399), "预算"},
		{"parent", strings.Repeat("甲", 1599), "预算"},
		{"long_phrase", strings.Repeat("甲", 1593), "数据库预算审批与回滚方案"},
		{"ascii_word", strings.Repeat("甲", 1599) + " ", "PostgreSQLBackup"},
		{"fullwidth", strings.Repeat("甲", 1599), "ＡＩ预算"},
		{"supplementary_han", strings.Repeat("甲", 1599), "𠮷野家"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks, err := BuildParentChild(uuid.New(), []Section{{Text: tc.prefix + tc.query + " " + strings.Repeat("乙", 1700)}})
			if err != nil {
				t.Fatal(err)
			}
			if !lexicalChildContains(chunks.Children, tc.query) {
				t.Fatalf("boundary term %q disappeared from searchable children", tc.query)
			}
		})
	}
}

// Verify source coverage independently of the splitter's chosen boundaries.
// Every window must be an exact advancing source slice, with no gap and at
// most 50 estimated tokens shared with its predecessor.
func assertParentWindowsCover(t *testing.T, text string, parents []Parent) {
	t.Helper()
	source := []rune(text)
	previous, covered := -1, 0
	for _, parent := range parents {
		length := len([]rune(parent.Content))
		found := false
		minimum := covered
		for minimum > previous+1 && EstimateTokens(string(source[minimum-1:covered])) <= 50 {
			minimum--
		}
		for start := minimum; start <= covered; start++ {
			end := start + length
			if end <= covered || end > len(source) || EstimateTokens(string(source[start:covered])) > 50 {
				continue
			}
			if string(source[start:end]) == parent.Content {
				previous, covered, found = start, end, true
				break
			}
		}
		if !found {
			t.Fatal("parent is not an advancing, bounded source window", parent.Ordinal)
		}
	}
	if covered != len(source) {
		t.Fatal("parent windows dropped source tail", covered, len(source))
	}
}

func TestParentChildLongWhitespaceKeepsExactChildren(t *testing.T) {
	source := strings.Repeat("甲", 400) + strings.Repeat(" \t", 20000) + "边界预算" + strings.Repeat("乙", 450)
	chunks, err := BuildParentChild(uuid.New(), []Section{{Text: source}})
	if err != nil || len(chunks.Parents) != 1 || !lexicalChildContains(chunks.Children, "边界预算") {
		t.Fatal("whitespace lost source text or failed to advance", err)
	}
	for _, child := range chunks.Children {
		if child.TokenCount > 400 || child.Content != string([]rune(chunks.Parents[0].Content)[child.ParentStart:child.ParentEnd]) {
			t.Fatal("whitespace changed child content, offsets or budget", child.Ordinal)
		}
	}
}

func TestParentChildUnbrokenChineseKeepsEveryBigram(t *testing.T) {
	runes := make([]rune, 3600)
	for i := range runes {
		runes[i] = rune(0x4e00 + i)
	}
	chunks, err := BuildParentChild(uuid.New(), []Section{{Text: string(runes)}})
	if err != nil {
		t.Fatal(err)
	}
	assertParentWindowsCover(t, string(runes), chunks.Parents)
	for i := 0; i+1 < len(runes); i++ {
		query := string(runes[i : i+2])
		found := false
		for _, child := range chunks.Children {
			found = found || strings.Contains(child.Content, query)
		}
		if !found {
			t.Fatalf("unbroken Chinese lost boundary bigram %q at %d", query, i)
		}
	}
}

func TestParentChildOverlapRetainsPageAndHeadingBoundaries(t *testing.T) {
	firstPage, secondPage := 1, 2
	blocks := []Section{
		{Text: strings.Repeat("甲", 3500), HeadingPath: "第一章", PageNumber: &firstPage},
		{Text: strings.Repeat("乙", 3500), HeadingPath: "第一章", PageNumber: &secondPage},
		{Text: strings.Repeat("丙", 3500), HeadingPath: "第二章", PageNumber: &secondPage},
	}
	chunks, err := BuildParentChild(uuid.New(), blocks)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range chunks.Children {
		if child.BlockStart != child.BlockEnd || child.BlockStart < 0 || child.BlockStart >= len(blocks) {
			t.Fatal("overlap crossed a parser location", child)
		}
		block := blocks[child.BlockStart]
		if !strings.Contains(block.Text, child.Content) || child.Location.PageNumber != block.PageNumber || child.Heading != block.HeadingPath {
			t.Fatal("overlap changed source text, page or heading", child)
		}
	}
}

func TestParentChildLongTableRowKeepsEveryChineseBigram(t *testing.T) {
	runes := make([]rune, 2000)
	for i := range runes {
		runes[i] = rune(0x4e00 + i)
	}
	block := Section{Text: "| 名称 |\n| --- |\n| " + string(runes) + " |", Kind: KindTable, HasHeader: true, SheetName: "预算", RowStart: 2, RowEnd: 2, ColumnStart: 1, ColumnEnd: 1}
	chunks, err := BuildParentChild(uuid.New(), []Section{block})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(runes); i++ {
		query := string(runes[i : i+2])
		if !lexicalChildContains(chunks.Children, query) {
			t.Fatalf("table row lost boundary bigram %q at %d", query, i)
		}
	}
	for _, child := range chunks.Children {
		if child.Location.RowStart != 2 || child.Location.RowEnd != 2 || child.Location.SheetName != "预算" || !strings.Contains(child.Content, "名称") || child.TokenCount > 600 {
			t.Fatal("table overlap changed header, row provenance or token bound", child)
		}
	}
}

func TestParentChildFrozenArtifactDigests(t *testing.T) {
	version := uuid.MustParse("94e03312-7d22-435e-a552-0d6e8105635a")
	blocks := []Section{
		{Text: ""},
		{Text: strings.Repeat("甲", 1599) + "预算🙂" + strings.Repeat("乙", 1700), HeadingPath: "章节甲"},
		{Text: "| 名称 | 数值 |\n| --- | --- |\n| " + strings.Repeat("中", 2000) + " | 1 |", Kind: KindTable, HasHeader: true, RowStart: 2, RowEnd: 2},
		{Text: "| 标题 |\n| --- |\n| 内容 |", Kind: KindTable, HasHeader: true, RowStart: 2, RowEnd: 2},
	}
	for _, tc := range []struct{ version, digest string }{
		{"parent-child-v1", "a22bff8456eb4e341b8deeaf3088e350dbae028c6eaa384a04b0d88eaec94864"},
		{"parent-child-v2", "93203e1c25e0a554468eb8fb6872c44976544eafc06bfb873230bd4a2226b2c4"},
		{"parent-child-v3", "08c3b4a8a1ce769aef36456fca374032c4260f3bf85eef02c3203136fe11f218"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			chunks, err := BuildParentChildVersion(version, blocks, tc.version)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(chunks)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(body)
			if got := hex.EncodeToString(digest[:]); got != tc.digest {
				t.Fatalf("frozen artifact changed: %s", got)
			}
		})
	}
}
