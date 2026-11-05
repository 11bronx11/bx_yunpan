package textsearch

import (
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPostgresLexicalContract(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	for _, text := range []string{
		"智能云盘支持中文搜索 PostgreSQL。",
		"ＰＧ，ＥＳ：中文 搜索，单字盘。",
		"①号 ＡＩ 文件 〇年 𠀀𠀁罕见字",
		"école e\u0301cole ΣΕΛΊΔΑ русский 日本語 한글",
		"foo_bar 1.25 user@example.com https://example.com/path",
		strings.Repeat("超长", 800),
		strings.Repeat("A", 500),
		"，。！？ 🙂\n\t",
	} {
		for _, query := range []bool{false, true} {
			var got string
			if err := db.Raw("SELECT ai_search_terms(?, ?)", text, query).Scan(&got).Error; err != nil {
				t.Fatal(err)
			}
			want := IndexText(text)
			if query {
				want = QueryText(text)
			}
			if got != want {
				t.Fatalf("query=%v source=%q: PG=%q Go=%q", query, text, got, want)
			}
		}
	}
}
