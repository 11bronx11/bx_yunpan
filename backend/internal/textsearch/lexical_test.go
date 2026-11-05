package textsearch

import (
	"strings"
	"testing"
)

func TestLexicalNormalizationMatchesEquivalentQueries(t *testing.T) {
	for _, pair := range [][2]string{
		{"ＰｏｓｔｇｒｅＳＱＬ 中文搜索", "postgresql 中文搜索"},
		{"①号 ＡＩ 文件", "1号 ai 文件"},
		{"école 中文", "e\u0301cole 中文"},
		{"中文，搜索：PG/ES", "中文 搜索 pg es"},
	} {
		if QueryText(pair[0]) != QueryText(pair[1]) {
			t.Fatalf("equivalent queries differ: %q / %q", pair[0], pair[1])
		}
	}
}

func TestLexicalChineseMatchingRejectsScatteredCharacters(t *testing.T) {
	query := strings.Fields(QueryText("中文搜索"))
	for _, test := range []struct {
		text string
		want bool
	}{
		{"智能云盘支持中文搜索功能。", true},
		{"中间的文字包含搜集资料和线索。", false},
	} {
		index := " " + IndexText(test.text) + " "
		matched := true
		for _, term := range query {
			matched = matched && strings.Contains(index, " "+term+" ")
		}
		if matched != test.want {
			t.Fatalf("text=%q matched=%v, want %v", test.text, matched, test.want)
		}
	}
	if !strings.Contains(" "+IndexText("云盘")+" ", " "+QueryText("盘")+" ") {
		t.Fatal("single-character query disappeared inside a Han run")
	}
	if QueryText("，。！？") != "" {
		t.Fatal("punctuation became an index term")
	}
}

func TestLexicalKeysSurviveNativeTokenizers(t *testing.T) {
	for _, term := range strings.Fields(IndexText("罕见汉字𠀀〇 " + strings.Repeat("x", 1000) + " PostgreSQL")) {
		if len(term) > 193 {
			t.Fatalf("term exceeds native analyzer limit: %d", len(term))
		}
		for _, r := range term {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				t.Fatalf("term can be split again: %q", term)
			}
		}
	}
}
