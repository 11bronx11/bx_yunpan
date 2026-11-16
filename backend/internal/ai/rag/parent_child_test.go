package rag

import (
	"github.com/google/uuid"
	"strings"
	"testing"
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
