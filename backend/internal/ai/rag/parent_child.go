package rag

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const ParentChildVersion = "parent-child-v3"

var ErrParentChildResourceLimit = errors.New("parent-child resource limit")

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
	return BuildParentChildVersion(version, blocks, ParentChildVersion)
}

func BuildParentChildVersion(version uuid.UUID, blocks []Section, implementation string) (ParentChild, error) {
	if !IsParentChildVersion(implementation) {
		return ParentChild{}, fmt.Errorf("unsupported parent-child version")
	}
	groups := groupParentBlocks(blocks, implementation != "parent-child-v1")
	out := ParentChild{Coverage: "complete"}
	for ordinal, group := range groups {
		if implementation == "parent-child-v1" {
			ordinal = group.start
		}
		block := group.Section
		text := strings.TrimSpace(strings.ReplaceAll(block.Text, "\r\n", "\n"))
		if text == "" {
			continue
		}
		if implementation != "parent-child-v1" && typedSectionKind(block) == KindTable {
			heading := block.HeadingPath
			if heading == "" {
				heading = block.Section
			}
			// Frozen v2 keeps its table output; v3 omits the header-only tail
			// produced after a long final row has already been emitted in pieces.
			pieces, incomplete := splitTypedTableTail(block, heading, text, ChunkOptions{TokenBudget: 600}, implementation == "parent-child-v2")
			if incomplete {
				return ParentChild{}, ErrParentChildResourceLimit
			}
			for n, piece := range pieces {
				if EstimateTokens(piece.Content) > 600 {
					return ParentChild{}, ErrParentChildResourceLimit
				}
				key := fmt.Sprintf("%s:%d:table:%d:%s", version, ordinal, n, implementation)
				location := block
				location.RowStart = piece.RowStart
				location.RowEnd = piece.RowEnd
				parent := Parent{ID: uuid.NewSHA1(version, []byte(key)), Ordinal: len(out.Parents), Key: key, Content: piece.Content, Heading: heading, Kind: KindTable, BlockStart: group.start, BlockEnd: group.end, TokenCount: EstimateTokens(piece.Content), Location: location}
				out.Parents = append(out.Parents, parent)
				out.Children = append(out.Children, Child{ID: uuid.NewSHA1(parent.ID, []byte(key+":child")), ParentID: parent.ID, Ordinal: len(out.Children), Key: key + ":child", Content: piece.Content, Heading: heading, BlockStart: group.start, BlockEnd: group.end, ParentStart: 0, ParentEnd: len([]rune(piece.Content)), TokenCount: parent.TokenCount, Location: location})
			}
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
			key := fmt.Sprintf("%s:%d:%d:%d:%s", version, ordinal, start, end, implementation)
			p := Parent{ID: uuid.NewSHA1(version, []byte(key)), Ordinal: len(out.Parents), Key: key, Content: content, Heading: heading, Kind: kind, BlockStart: group.start, BlockEnd: group.end, TokenCount: EstimateTokens(content), Location: block}
			out.Parents = append(out.Parents, p)
			pr := []rune(content)
			for cs := 0; cs < len(pr); {
				ce := boundedEnd(pr, cs, 400, 600)
				ck := fmt.Sprintf("%s:%d:%d", key, cs, ce)
				out.Children = append(out.Children, Child{ID: uuid.NewSHA1(p.ID, []byte(ck)), ParentID: p.ID, Ordinal: len(out.Children), Key: ck, Content: string(pr[cs:ce]), Heading: heading, BlockStart: group.start, BlockEnd: group.end, ParentStart: cs, ParentEnd: ce, TokenCount: EstimateTokens(string(pr[cs:ce])), Location: block})
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

func IsParentChildVersion(version string) bool {
	return version == "parent-child-v1" || version == "parent-child-v2" || version == ParentChildVersion
}

// ContextualText adds the section context separately from exact child/parent
// ranges. A budget of zero preserves all terms for lexical indexing; model
// inputs reserve space for the actual passage and cap the heading prefix.
func ContextualText(heading, content string, budget int) string {
	heading = strings.TrimSpace(heading)
	if heading == "" || content == heading || strings.HasPrefix(content, heading+"\n") {
		return content
	}
	if budget > 0 {
		available := min(160, budget-EstimateTokens(content))
		if available <= 0 {
			return content
		}
		r := []rune(heading)
		heading = strings.TrimSpace(string(r[:fittingTokenEnd(r, 0, available)]))
	}
	return heading + "\n\n" + content
}

func boundedEnd(r []rune, start, target, hard int) int {
	end := fittingTokenEnd(r, start, min(target, hard))
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

type parentBlock struct {
	Section
	start, end int
}

func groupParentBlocks(blocks []Section, merge bool) []parentBlock {
	var groups []parentBlock
	for i, b := range blocks {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		if merge && len(groups) > 0 {
			p := &groups[len(groups)-1]
			separator := "\n\n"
			if typedSectionKind(b) == KindList {
				separator = "\n"
			}
			combined := strings.TrimSpace(p.Text) + separator + strings.TrimSpace(b.Text)
			if canCoalesceTypedSections(p.Section, b) && EstimateTokens(combined) <= 1600 {
				p.Text = combined
				p.end = i
				// The combined location remains a block range, never invented character offsets.
				p.SourceEnd = max(p.SourceEnd, b.SourceEnd)
				continue
			}
		}
		groups = append(groups, parentBlock{Section: b, start: i, end: i})
	}
	return groups
}
