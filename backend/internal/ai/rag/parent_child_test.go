package rag

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParentChildCoverageAndStableRanges(t *testing.T) {
	source := []Section{{Text: strings.Repeat("重复标题\n边界测试 中文。alpha beta gamma\n", 700), HeadingPath: "重复标题"}, {Text: "尾部独有风险：请保留回滚日志", HeadingPath: "重复标题"}}
	v := uuid.New()
	got, err := BuildParentChild(v, source)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := BuildParentChild(v, source)
	if len(got.Parents) < 3 || len(got.Children) < len(got.Parents) {
		t.Fatal("missing splits")
	}
	for i, p := range got.Parents {
		if p.ID != again.Parents[i].ID || p.TokenCount > 2400 {
			t.Fatal("unstable/unbounded parent")
		}
		covered := make([]bool, len([]rune(p.Content)))
		for _, c := range got.Children {
			if c.ParentID != p.ID {
				continue
			}
			if string([]rune(p.Content)[c.ParentStart:c.ParentEnd]) != c.Content || c.TokenCount > 600 {
				t.Fatal("invalid child range")
			}
			for j := c.ParentStart; j < c.ParentEnd; j++ {
				covered[j] = true
			}
		}
		for _, c := range covered {
			if !c {
				t.Fatal("coverage gap")
			}
		}
	}
	if !strings.Contains(got.Parents[len(got.Parents)-1].Content, "尾部独有风险") {
		t.Fatal("tail dropped")
	}
	parts := SummaryParts(source, 1000)
	if !strings.Contains(strings.Join(parts, ""), "尾部独有风险") {
		t.Fatal("summary dropped tail")
	}
}

func TestParentChildGroupsParagraphsWithoutCrossingLocations(t *testing.T) {
	p1, p2 := 1, 2
	blocks := []Section{{Text: "第一段是原因。", HeadingPath: "背景", PageNumber: &p1}, {Text: "第二段是结论。", HeadingPath: "背景", PageNumber: &p1}, {Text: "新页", HeadingPath: "背景", PageNumber: &p2}, {Text: "列1,列2\n甲,乙", Kind: KindTable, HeadingPath: "背景", PageNumber: &p2}, {Text: "另一节", HeadingPath: "结论", PageNumber: &p2}}
	got, e := BuildParentChild(uuid.New(), blocks)
	if e != nil || len(got.Parents) != 4 {
		t.Fatal("paragraph grouping lost boundaries", got, e)
	}
	if got.Parents[0].BlockStart != 0 || got.Parents[0].BlockEnd != 1 || !strings.Contains(got.Parents[0].Content, "第二段") {
		t.Fatal("paragraph context lost")
	}
	old, e := BuildParentChildVersion(uuid.New(), blocks, "parent-child-v1")
	if e != nil || len(old.Parents) != 5 {
		t.Fatal("old frozen chunker changed")
	}
}

func TestParentChildLargeMixedTextRemainsBounded(t *testing.T) {
	text := strings.Repeat("界面中文🙂 abcdefghijklmnopqrstuvwxyz0123456789。\n", 10000) + "最后唯一标志TAIL937"
	got, e := BuildParentChild(uuid.New(), []Section{{Text: text}})
	if e != nil {
		t.Fatal(e)
	}
	var joined strings.Builder
	for _, p := range got.Parents {
		joined.WriteString(p.Content)
		if p.TokenCount > 2400 {
			t.Fatal("unbounded parent")
		}
	}
	if joined.String() != text {
		t.Fatal("source text skipped or repeated")
	}
	for _, c := range got.Children {
		if c.TokenCount > 600 {
			t.Fatal("unbounded child")
		}
	}
}

func TestParentChildTablesRetainHeadersAndRowLocations(t *testing.T) {
	var table strings.Builder
	table.WriteString("| 设备编号 | 项目预算 |\n| --- | --- |\n")
	for i := 2; i <= 81; i++ {
		fmt.Fprintf(&table, "| BX%04d | %s |\n", i, strings.Repeat("中", 90))
	}
	got, e := BuildParentChild(uuid.New(), []Section{{Text: table.String(), Kind: KindTable, HasHeader: true, SheetName: "预算表", RowStart: 2, RowEnd: 81, ColumnStart: 1, ColumnEnd: 2}})
	if e != nil || len(got.Parents) < 10 {
		t.Fatal("table did not split", e)
	}
	seen := map[int]bool{}
	for _, p := range got.Parents {
		if !strings.Contains(p.Content, "设备编号") || p.TokenCount > 600 || p.Location.SheetName != "预算表" {
			t.Fatal("table context lost", p)
		}
		for row := p.Location.RowStart; row <= p.Location.RowEnd; row++ {
			if strings.Contains(p.Content, fmt.Sprintf("BX%04d", row)) {
				seen[row] = true
			}
		}
	}
	for row := 2; row <= 81; row++ {
		if !seen[row] {
			t.Fatalf("row %d lost or mislocated", row)
		}
	}
	for _, c := range got.Children {
		if c.ParentStart != 0 || c.ParentEnd != len([]rune(c.Content)) || !strings.Contains(c.Content, "设备编号") {
			t.Fatal("table child lost header/range")
		}
	}
}

func TestFrozenParentChildV1KeepsIDsAcrossEmptyBlocks(t *testing.T) {
	version := uuid.New()
	got, e := BuildParentChildVersion(version, []Section{{Text: ""}, {Text: "abc"}}, "parent-child-v1")
	key := fmt.Sprintf("%s:1:0:3:parent-child-v1", version)
	if e != nil || len(got.Parents) != 1 || got.Parents[0].ID != uuid.NewSHA1(version, []byte(key)) || got.Parents[0].BlockStart != 1 {
		t.Fatal("frozen v1 identity changed across empty block", e, got)
	}
}
