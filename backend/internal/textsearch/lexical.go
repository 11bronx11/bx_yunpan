// Package textsearch defines the lexical representation shared by the search
// writers and readers. Original parser/chunk text is never normalized in place.
package textsearch

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const Version = "zh-lexical-v1"

// Normalize is mirrored by lower(normalize(value, NFKC)) in ai_search_terms.
func Normalize(value string) string {
	return strings.ToLower(norm.NFKC.String(value))
}

func IndexText(value string) string { return analyze(value, false) }
func QueryText(value string) string { return analyze(value, true) }

func analyze(value string, query bool) string {
	var terms []string
	var run []rune
	kind := 0
	flush := func() {
		if len(run) == 0 {
			return
		}
		if kind == 1 {
			for i, r := range run {
				if !query || len(run) == 1 {
					terms = append(terms, termKey("h", string(r)))
				}
				if i+1 < len(run) {
					terms = append(terms, termKey("h", string(run[i:i+2])))
				}
			}
		} else {
			terms = append(terms, termKey("w", string(run)))
		}
		run = run[:0]
	}
	for _, r := range Normalize(value) {
		next := 0
		if hanRune(r) {
			next = 1
		} else if unicode.IsLetter(r) || unicode.IsNumber(r) {
			next = 2
		}
		if next != kind || next == 0 {
			flush()
		}
		kind = next
		if next != 0 {
			run = append(run, r)
		}
	}
	flush()
	return strings.Join(terms, " ")
}

// These ranges, token order, and key encoding are mirrored by migration 31.
// Integration tests compare both representations before exercising PG and ES.
func hanRune(r rune) bool {
	return r == '〇' || r >= 0x3400 && r <= 0x4dbf || r >= 0x4e00 && r <= 0x9fff ||
		r >= 0xf900 && r <= 0xfaff || r >= 0x20000 && r <= 0x323af
}

func termKey(kind, value string) string {
	// ASCII keys prevent either engine's native tokenizer from splitting our
	// tokens again. Keep keys below even ES standard's 255-character limit,
	// including writes to legacy epochs with dynamically mapped text fields.
	if len(value) > 96 {
		digest := sha256.Sum256([]byte(kind + value))
		return "x" + hex.EncodeToString(digest[:])
	}
	return kind + hex.EncodeToString([]byte(value))
}
