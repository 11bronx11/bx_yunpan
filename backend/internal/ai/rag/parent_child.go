package rag

import (
	"fmt"
	"github.com/google/uuid"
	"strings"
	"unicode/utf8"
)

const ParentChildVersion = "parent-child-v1"

type Parent struct {
	ID                               uuid.UUID
	Ordinal                          int
	Key, Content, Heading, Kind      string
	BlockStart, BlockEnd, TokenCount int
	Location                         Section
}
type Child struct {
	ID, ParentID                                             uuid.UUID
	Ordinal                                                  int
	Key, Content, Heading                                    string
	BlockStart, BlockEnd, ParentStart, ParentEnd, TokenCount int
	Location                                                 Section
}
type ParentChild struct {
	Parents  []Parent
	Children []Child
	Coverage string
}

// BuildParentChild consumes canonical blocks exactly once. Overlap belongs only
// to children and all child offsets are exact rune positions in their parent.
// Parser offsets remain block precision unless the parser proves otherwise.
func BuildParentChild(version uuid.UUID, blocks []Section) (ParentChild, error) {
	out := ParentChild{Coverage: "complete"}
	for ordinal, block := range blocks {
		text := strings.TrimSpace(strings.ReplaceAll(block.Text, "\r\n", "\n"))
		if text == "" {
			continue
		}
		runes := []rune(text)
		for start := 0; start < len(runes); {
			end := boundedEnd(runes, start, 1600, 2400)
			content := string(runes[start:end])
			heading := block.HeadingPath
			if heading == "" {
				heading = block.Section
			}
			kind := block.Kind
			if kind == "" {
				kind = "text"
			}
			key := fmt.Sprintf("%s:%d:%d:%d:%s", version, ordinal, start, end, ParentChildVersion)
			p := Parent{ID: uuid.NewSHA1(version, []byte(key)), Ordinal: len(out.Parents), Key: key, Content: content, Heading: heading, Kind: kind, BlockStart: ordinal, BlockEnd: ordinal, TokenCount: EstimateTokens(content), Location: block}
			out.Parents = append(out.Parents, p)
			pr := []rune(content)
			for cs := 0; cs < len(pr); {
				ce := boundedEnd(pr, cs, 400, 600)
				ck := fmt.Sprintf("%s:%d:%d", key, cs, ce)
				out.Children = append(out.Children, Child{ID: uuid.NewSHA1(p.ID, []byte(ck)), ParentID: p.ID, Ordinal: len(out.Children), Key: ck, Content: string(pr[cs:ce]), Heading: heading, BlockStart: ordinal, BlockEnd: ordinal, ParentStart: cs, ParentEnd: ce, TokenCount: EstimateTokens(string(pr[cs:ce])), Location: block})
				if ce == len(pr) {
					break
				}
				next := ce
				for next > cs+1 && EstimateTokens(string(pr[next-1:ce])) < 50 {
					next--
				}
				cs = max(cs+1, next)
			}
			start = end
		}
	}
	if len(out.Children) == 0 {
		out.Coverage = "none"
	}
	return out, nil
}
func boundedEnd(r []rune, start, target, hard int) int {
	end := start
	for end < len(r) && EstimateTokens(string(r[start:end+1])) <= target {
		end++
	}
	if end == len(r) {
		return end
	}
	if end == start {
		return start + 1
	}
	// Prefer a nearby sentence/line boundary without discarding source text.
	for i := end; i > start+(end-start)*2/3; i-- {
		if strings.ContainsRune("\n。！？.!?;；", r[i-1]) {
			return i
		}
	}
	_ = hard
	return end
}

// SummaryParts covers every source byte without deriving text from overlapping
// children. Calls are bounded by the caller; no silent head-only summary.
func SummaryParts(blocks []Section, budget int) []string {
	var parts []string
	var pending strings.Builder
	flush := func() {
		if pending.Len() > 0 {
			parts = append(parts, pending.String())
			pending.Reset()
		}
	}
	for _, b := range blocks {
		r := []rune(b.Text)
		for start := 0; start < len(r); {
			end := boundedEnd(r, start, budget, budget)
			piece := string(r[start:end])
			if EstimateTokens(pending.String()+piece) > budget {
				flush()
			}
			pending.WriteString(piece)
			pending.WriteString("\n")
			start = end
		}
	}
	flush()
	return parts
}

var _ = utf8.RuneCountInString
