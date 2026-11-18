package aisystem

import (
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/textsearch"
)

// Exercise the actual migrated SQL functions, including adversarial negatives.
func TestChineseLexicalParity(t *testing.T) {
	db, _ := databases(t)
	for _, tc := range []struct {
		text, query string
		match       bool
	}{
		{"项目预算已经批准", "项目预算", true},
		{"项目预算已经批准", "目", true},
		{"项目已启动，预算尚未批准", "项目预算", false},
		{"项目预算已经批准", "项目 天气", false},
		{"ＡＩ 数据库 PostgreSQL 备份", "ai 数据库 POSTGRESQL", true},
		{"𠮷野家预算", "𠮷野", true},
		{"项目预算", "!!!", false},
	} {
		var got bool
		if err := db.Raw("SELECT ai_fts_text(?) @@ ai_fts_query(?)", tc.text, tc.query).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != tc.match {
			t.Errorf("%q / %q: got %t want %t", tc.text, tc.query, got, tc.match)
		}
		var sqlTerms string
		// Keep PG and the independently deployed ES writer on one representation.
		if err := db.Raw("SELECT ai_search_terms(?,true)", tc.query).Scan(&sqlTerms).Error; err != nil {
			t.Error(err)
			continue
		}
		if sqlTerms != textsearch.QueryText(tc.query) {
			t.Errorf("PG/Go terms differ: %q / %q", sqlTerms, textsearch.QueryText(tc.query))
		}
	}
}
