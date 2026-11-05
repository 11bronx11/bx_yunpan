package search

import (
	"strings"
	"unicode"

	"github.com/11bronx11/bx_yunpan/backend/internal/textsearch"
	"golang.org/x/text/width"
)

func esTextQuery(query string) map[string]any {
	terms := textsearch.QueryText(query)
	if terms == "" {
		return map[string]any{"match_none": map[string]any{}}
	}
	return map[string]any{"bool": map[string]any{"minimum_should_match": 1, "should": []any{
		map[string]any{"bool": map[string]any{
			"must":   map[string]any{"match": map[string]any{"content_search": map[string]any{"query": terms, "operator": "and", "analyzer": "whitespace"}}},
			"should": map[string]any{"match_phrase": map[string]any{"content_normalized": map[string]any{"query": textsearch.Normalize(query), "boost": 4}}},
		}},
		// Compatibility is limited to documents not yet backfilled. It must
		// not broaden the canonical AND predicate for newly written documents.
		map[string]any{"bool": map[string]any{
			"must_not": map[string]any{"exists": map[string]any{"field": "content_search"}},
			"must":     esLegacyTextQuery(query),
		}},
	}}}
}

func esLegacyTextQuery(query string) map[string]any {
	query = width.Fold.String(query)
	// Require every query segment. The old default OR query admitted a whole
	// document after matching a single common Chinese character. On legacy
	// standard fields each Han run must occur as a phrase, preserving order.
	var required []any
	var segment strings.Builder
	han := false
	flush := func() {
		text := segment.String()
		segment.Reset()
		if !strings.ContainsFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			return
		}
		if han {
			required = append(required, map[string]any{"match_phrase": map[string]any{"content": text}})
		} else {
			required = append(required, map[string]any{"match": map[string]any{"content": map[string]any{"query": text, "operator": "and"}}})
		}
	}
	for _, r := range query {
		nextHan := unicode.Is(unicode.Han, r)
		if nextHan != han {
			flush()
			han = nextHan
		}
		segment.WriteRune(r)
	}
	flush()
	if len(required) == 0 {
		return map[string]any{"match_none": map[string]any{}}
	}
	return map[string]any{"dis_max": map[string]any{"queries": []any{
		map[string]any{"bool": map[string]any{"must": required}},
		map[string]any{"match": map[string]any{"content.cjk": map[string]any{"query": query, "operator": "and", "boost": 2}}},
		map[string]any{"match_phrase": map[string]any{"content": map[string]any{"query": query, "boost": 4}}},
	}}}
}
