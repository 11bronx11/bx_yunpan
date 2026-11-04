package rag

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxChunksPerDocument = 256
	ChunkSize            = 900
	ChunkOverlap         = 120

	KindText          = "text"
	KindTable         = "table"
	KindCode          = "code"
	KindList          = "list"
	KindVisionSummary = "vision_summary"

	SourceKindNative      = "native"
	SourceKindCloudVision = "cloud_vision"
)

var (
	markdownHeadingPattern = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	listItemPattern        = regexp.MustCompile(`^\s*(?:[-+*]|\d+[.)])\s+`)
)

// Section is extracted content plus the source location and structure retained
// by a parser. Kind is optional so older parser implementations remain valid.
type Section struct {
	Text         string   `json:"text"`
	SourceKind   string   `json:"source_kind,omitempty"`
	PageNumber   *int     `json:"page_number,omitempty"`
	PageEnd      int      `json:"page_end,omitempty"`
	Section      string   `json:"section,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	HeadingPath  string   `json:"heading_path,omitempty"`
	SlideNumber  int      `json:"slide_number,omitempty"`
	SheetName    string   `json:"sheet_name,omitempty"`
	RowStart     int      `json:"row_start,omitempty"`
	RowEnd       int      `json:"row_end,omitempty"`
	ColumnStart  int      `json:"column_start,omitempty"`
	ColumnEnd    int      `json:"column_end,omitempty"`
	SourceStart  int64    `json:"source_start,omitempty"`
	SourceEnd    int64    `json:"source_end,omitempty"`
	HasHeader    bool     `json:"has_header,omitempty"`
	MergedRanges []string `json:"merged_ranges,omitempty"`
	Encoding     string   `json:"encoding,omitempty"`
}

// Chunk is the indexable unit produced from an extracted section.
type Chunk struct {
	Content              string
	SourceKind           string
	PageNumber           *int
	Section              string
	Kind                 string
	HeadingPath          string
	PageStart            int
	PageEnd              int
	SlideNumber          int
	SheetName            string
	RowStart             int
	RowEnd               int
	ColumnStart          int
	ColumnEnd            int
	SourceStart          int64
	SourceEnd            int64
	TokenCount           int
	TokenCountIsEstimate bool
	MergedRanges         []string
	Encoding             string
}

type contentBlock struct {
	text    string
	kind    string
	heading string
}

// MakeChunks keeps document structure, repeats heading context, and overlaps
// neighboring chunks without crossing parser sections or source pages.
func MakeChunks(sections []Section) []Chunk {
	result := make([]Chunk, 0)
	for _, section := range sections {
		var pending, pendingHeading string
		flush := func() {
			if strings.TrimSpace(pending) == "" || len(result) >= MaxChunksPerDocument {
				pending = ""
				return
			}
			result = append(result, Chunk{
				Content: strings.TrimSpace(pending), PageNumber: section.PageNumber,
				Section: combineSection(section.Section, pendingHeading),
			})
			pending, pendingHeading = "", ""
		}

		for _, block := range blocksFor(section) {
			heading := strings.TrimSpace(block.heading)
			if pending != "" && heading != pendingHeading {
				flush()
			}
			if block.kind == KindTable || block.kind == KindCode || block.kind == KindList {
				flush()
				pieces := splitStructuredBlock(block)
				for _, piece := range pieces {
					result = appendBounded(result, Chunk{
						Content: decorate(heading, piece), PageNumber: section.PageNumber,
						Section: combineSection(section.Section, heading),
					})
					if len(result) >= MaxChunksPerDocument {
						return result
					}
				}
				continue
			}

			limit := contentLimit(heading)
			for pieceIndex, piece := range splitText(block.text, limit, textSeparators) {
				content := decorate(heading, piece)
				if pending == "" {
					pending, pendingHeading = content, heading
					continue
				}
				joined := pending + "\n\n" + content
				if runeCount(joined) <= ChunkSize {
					pending = joined
					continue
				}
				previous := pending
				flush()
				pendingHeading = heading
				if pieceIndex == 0 {
					pending = joinOverlap(overlapTail(previous, ChunkOverlap), content, ChunkSize)
				} else {
					pending = content
				}
			}
			if len(result) >= MaxChunksPerDocument {
				return result
			}
		}
		flush()
		if len(result) >= MaxChunksPerDocument {
			return result
		}
	}
	return result
}

func appendBounded(chunks []Chunk, chunk Chunk) []Chunk {
	if len(chunks) >= MaxChunksPerDocument {
		return chunks
	}
	if runeCount(chunk.Content) > ChunkSize {
		for _, piece := range splitText(chunk.Content, ChunkSize, textSeparators) {
			chunks = append(chunks, Chunk{Content: piece, PageNumber: chunk.PageNumber, Section: chunk.Section})
			if len(chunks) >= MaxChunksPerDocument {
				return chunks
			}
		}
		return chunks
	}
	return append(chunks, chunk)
}

func blocksFor(section Section) []contentBlock {
	text := normalizeLines(section.Text)
	switch section.Kind {
	case KindTable, KindCode, KindList:
		return []contentBlock{{text: text, kind: section.Kind}}
	}
	return parseBlocks(text)
}

func parseBlocks(text string) []contentBlock {
	lines := strings.Split(text, "\n")
	var headings [6]string
	var paragraph, list []string
	blocks := make([]contentBlock, 0)
	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}
		blocks = append(blocks, contentBlock{text: strings.Join(paragraph, " "), kind: KindText, heading: headingPath(headings)})
		paragraph = nil
	}
	flushList := func() {
		if len(list) == 0 {
			return
		}
		blocks = append(blocks, contentBlock{text: strings.Join(list, "\n"), kind: KindList, heading: headingPath(headings)})
		list = nil
	}
	flushPlain := func() {
		flushParagraph()
		flushList()
	}

	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			flushPlain()
			continue
		}
		if heading := markdownHeadingPattern.FindStringSubmatch(line); heading != nil {
			flushPlain()
			level := len(heading[1])
			headings[level-1] = strings.TrimSpace(heading[2])
			for lower := level; lower < len(headings); lower++ {
				headings[lower] = ""
			}
			continue
		}
		if isCodeFence(line) {
			flushPlain()
			code := []string{line}
			for index++; index < len(lines); index++ {
				code = append(code, lines[index])
				if isClosingCodeFence(strings.TrimSpace(lines[index]), line) {
					break
				}
			}
			blocks = append(blocks, contentBlock{text: strings.Join(code, "\n"), kind: KindCode, heading: headingPath(headings)})
			continue
		}
		if isMarkdownTableStart(lines, index) {
			flushPlain()
			rows := []string{line}
			for index+1 < len(lines) && isMarkdownTableRow(lines[index+1]) {
				index++
				rows = append(rows, strings.TrimSpace(lines[index]))
			}
			blocks = append(blocks, contentBlock{text: strings.Join(rows, "\n"), kind: KindTable, heading: headingPath(headings)})
			continue
		}
		if listItemPattern.MatchString(line) {
			flushParagraph()
			list = append(list, line)
			continue
		}
		flushList()
		paragraph = append(paragraph, line)
	}
	flushPlain()
	return blocks
}

func splitStructuredBlock(block contentBlock) []string {
	heading := strings.TrimSpace(block.heading)
	limit := contentLimit(heading)
	switch block.kind {
	case KindTable:
		return splitTable(block.text, limit)
	case KindCode:
		return splitText(block.text, limit, codeSeparators)
	case KindList:
		return splitText(block.text, limit, listSeparators)
	default:
		return splitText(block.text, limit, textSeparators)
	}
}

func splitTable(text string, limit int) []string {
	lines := nonEmptyLines(text)
	if len(lines) < 2 || runeCount(strings.Join(lines[:minInt(2, len(lines))], "\n")) > limit {
		return splitText(strings.Join(lines, "\n"), limit, tableSeparators)
	}
	headerLines := 1
	if len(lines) > 1 && isTableSeparator(lines[1]) {
		headerLines = 2
	}
	header := strings.Join(lines[:headerLines], "\n")
	if runeCount(header) >= limit {
		return splitText(strings.Join(lines, "\n"), limit, tableSeparators)
	}

	result := make([]string, 0)
	rows := make([]string, 0)
	flush := func() {
		if len(rows) == 0 {
			return
		}
		result = append(result, header+"\n"+strings.Join(rows, "\n"))
		rows = nil
	}
	for _, row := range lines[headerLines:] {
		candidate := header + "\n" + strings.Join(append(append([]string(nil), rows...), row), "\n")
		if runeCount(candidate) <= limit {
			rows = append(rows, row)
			continue
		}
		flush()
		if runeCount(header)+1+runeCount(row) <= limit {
			rows = append(rows, row)
			continue
		}
		for _, piece := range splitText(row, maxInt(1, limit-runeCount(header)-1), tableSeparators) {
			result = append(result, header+"\n"+piece)
		}
	}
	flush()
	if len(result) == 0 {
		result = append(result, header)
	}
	return result
}

var textSeparators = []string{"\n\n", "\n", "。", "！", "？", "!", "?", "；", ";", ".", " "}
var codeSeparators = []string{"\n", "\t", ",", ";", " "}
var listSeparators = []string{"\n", "。", "；", ";", " "}
var tableSeparators = []string{"\n", "|", ",", "\t", " "}

func splitText(text string, limit int, separators []string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if limit <= 0 {
		limit = 1
	}
	atoms := recursiveAtoms(text, limit, separators)
	return mergeAtoms(atoms, limit)
}

func recursiveAtoms(text string, limit int, separators []string) []string {
	if runeCount(text) <= limit {
		return []string{text}
	}
	for index, separator := range separators {
		parts := splitAfter(text, separator)
		if len(parts) < 2 {
			continue
		}
		result := make([]string, 0, len(parts))
		for _, part := range parts {
			if strings.TrimSpace(part) == "" {
				continue
			}
			if runeCount(part) <= limit {
				result = append(result, part)
			} else {
				result = append(result, recursiveAtoms(part, limit, separators[index+1:])...)
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return hardAtoms(text, limit)
}

func splitAfter(text, separator string) []string {
	if separator == "" || !strings.Contains(text, separator) {
		return nil
	}
	parts := strings.SplitAfter(text, separator)
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func mergeAtoms(atoms []string, limit int) []string {
	result := make([]string, 0)
	current := ""
	separatorAfterCurrent := ""
	for _, rawAtom := range atoms {
		atom := strings.TrimSpace(rawAtom)
		if atom == "" {
			continue
		}
		separator := separatorAfterCurrent + leadingWhitespace(rawAtom)
		separatorAfterCurrent = trailingWhitespace(rawAtom)
		if current == "" {
			current = atom
			continue
		}
		joined := current + separator + atom
		if runeCount(joined) <= limit {
			current = joined
			continue
		}
		result = append(result, current)
		current = joinOverlapWithSeparator(overlapTail(current, ChunkOverlap), separator, atom, limit)
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

func hardAtoms(text string, limit int) []string {
	runes := []rune(text)
	result := make([]string, 0, (len(runes)+limit-1)/limit)
	for start := 0; start < len(runes); start += limit {
		end := minInt(start+limit, len(runes))
		result = append(result, string(runes[start:end]))
	}
	return result
}

func overlapTail(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if limit <= 0 || len(runes) == 0 {
		return ""
	}
	start := maxInt(0, len(runes)-limit)
	for index := start + 1; index < len(runes); index++ {
		if overlapBoundary(runes[index-1]) {
			start = index
			break
		}
	}
	return strings.TrimSpace(string(runes[start:]))
}

func joinOverlap(overlap, content string, limit int) string {
	return joinOverlapWithSeparator(overlap, "\n", content, limit)
}

func joinOverlapWithSeparator(overlap, separator, content string, limit int) string {
	content = strings.TrimSpace(content)
	if limit <= 0 || runeCount(content) >= limit {
		return content
	}
	available := limit - runeCount(content)
	if runeCount(separator) > available {
		separator = string([]rune(separator)[:available])
		available = 0
	} else {
		available -= runeCount(separator)
	}
	if runeCount(overlap) > available {
		runes := []rune(overlap)
		overlap = string(runes[len(runes)-available:])
	}
	if overlap == "" {
		return content
	}
	return overlap + separator + content
}

func leadingWhitespace(value string) string {
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	return value[:len(value)-len(trimmed)]
}

func trailingWhitespace(value string) string {
	trimmed := strings.TrimRightFunc(value, unicode.IsSpace)
	return value[len(trimmed):]
}

func contentLimit(heading string) int {
	context := runeCount(heading)
	if context > ChunkSize/3 {
		context = ChunkSize / 3
	}
	return maxInt(1, ChunkSize-context-2)
}

func decorate(heading, text string) string {
	if heading == "" {
		return strings.TrimSpace(text)
	}
	return heading + "\n\n" + strings.TrimSpace(text)
}

func combineSection(source, heading string) string {
	source, heading = strings.TrimSpace(source), strings.TrimSpace(heading)
	if source == "" {
		return heading
	}
	if heading == "" {
		return source
	}
	return source + " / " + heading
}

func headingPath(headings [6]string) string {
	parts := make([]string, 0, len(headings))
	for level, heading := range headings {
		if heading != "" {
			parts = append(parts, strings.Repeat("#", level+1)+" "+heading)
		}
	}
	return strings.Join(parts, " > ")
}

func normalizeLines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

func isCodeFence(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

func isClosingCodeFence(line, opening string) bool {
	marker := strings.TrimSpace(opening)
	if strings.HasPrefix(marker, "```") {
		return strings.HasPrefix(line, "```")
	}
	return strings.HasPrefix(line, "~~~")
}

func isMarkdownTableStart(lines []string, index int) bool {
	return index+1 < len(lines) && isMarkdownTableRow(lines[index]) && isTableSeparator(lines[index+1])
}

func isMarkdownTableRow(line string) bool {
	return strings.Count(line, "|") >= 1
}

func isTableSeparator(line string) bool {
	line = strings.Trim(strings.TrimSpace(line), "|")
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		part = strings.TrimSpace(strings.Trim(part, ":"))
		if len(part) < 3 || strings.Trim(part, "-") != "" {
			return false
		}
	}
	return true
}

func nonEmptyLines(text string) []string {
	result := make([]string, 0)
	for _, line := range strings.Split(normalizeLines(text), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func overlapBoundary(value rune) bool {
	return value == '\n' || value == ' ' || strings.ContainsRune("。！？!?；;,.", value)
}

func runeCount(value string) int { return utf8.RuneCountInString(value) }

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
