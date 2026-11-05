package rag

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultTokenBudget = 600
	DefaultOverlap     = 60
	DefaultChunker     = "document-chunker-v3"
	EstimateTokenizer  = "mixed-script-estimate-v1"
)

type ChunkOptions struct {
	TokenBudget int
	Overlap     int
	MaxChunks   int
	Version     string
	TokenizerID string
}

type ChunkResult struct {
	Chunks      []Chunk
	Truncated   bool
	Warnings    []string
	Version     string
	TokenizerID string
}

// MakeTypedChunks applies a conservative token estimate while preserving the
// parser's typed source blocks. The estimate is intentionally named because
// text-embedding-v4 does not expose a compatible local tokenizer.
func MakeTypedChunks(sections []Section, options ChunkOptions) ChunkResult {
	options = normalizeChunkOptions(options)
	sections = coalesceTypedSections(sections)
	result := ChunkResult{
		Chunks: make([]Chunk, 0), Version: options.Version, TokenizerID: options.TokenizerID,
	}
	appendChunk := func(chunk Chunk) bool {
		if len(result.Chunks) >= options.MaxChunks {
			result.Truncated = true
			return false
		}
		chunk.TokenCount = EstimateTokens(chunk.Content)
		chunk.TokenCountIsEstimate = true
		result.Chunks = append(result.Chunks, chunk)
		return true
	}

	for _, section := range sections {
		if strings.TrimSpace(section.Text) == "" {
			continue
		}
		kind := typedSectionKind(section)
		heading := strings.TrimSpace(section.HeadingPath)
		text := normalizeLines(section.Text)
		if heading == "" {
			heading, text = splitLeadingHeading(text)
		}
		if kind == KindTable {
			chunks, incomplete := splitTypedTable(section, heading, text, options)
			if incomplete {
				result.Truncated = true
				result.Warnings = append(result.Warnings, "table header exceeds the configured token budget")
				return result
			}
			for _, chunk := range chunks {
				if !appendChunk(chunk) {
					return result
				}
			}
			continue
		}

		separators := typedTextSeparators
		switch kind {
		case KindCode:
			separators = typedCodeSeparators
		case KindList:
			separators = typedListSeparators
		}
		bodyBudget := options.TokenBudget - EstimateTokens(heading) - headingOverhead(heading)
		if bodyBudget < 1 {
			bodyBudget = 1
		}
		pieces := splitTextByTokens(text, bodyBudget, options.Overlap, separators)
		for _, piece := range pieces {
			content := addHeading(heading, piece, options.TokenBudget)
			chunk := chunkFromSection(section, kind, heading, content)
			if !appendChunk(chunk) {
				return result
			}
		}
	}
	return result
}

func coalesceTypedSections(sections []Section) []Section {
	if len(sections) < 2 {
		return sections
	}
	result := make([]Section, 0, len(sections))
	for _, section := range sections {
		if len(result) == 0 || !canCoalesceTypedSections(result[len(result)-1], section) {
			result = append(result, section)
			continue
		}
		previous := &result[len(result)-1]
		separator := "\n\n"
		if typedSectionKind(section) == KindList {
			separator = "\n"
		}
		previous.Text = strings.TrimSpace(previous.Text) + separator + strings.TrimSpace(section.Text)
		if previous.PageEnd == 0 {
			previous.PageEnd = section.PageEnd
		}
		if section.SourceStart < previous.SourceStart {
			previous.SourceStart = section.SourceStart
		}
		if section.SourceEnd > previous.SourceEnd {
			previous.SourceEnd = section.SourceEnd
		}
	}
	return result
}

func canCoalesceTypedSections(previous, next Section) bool {
	kind := typedSectionKind(previous)
	if kind != KindText && kind != KindList || kind != typedSectionKind(next) {
		return false
	}
	if strings.TrimSpace(previous.HeadingPath) != strings.TrimSpace(next.HeadingPath) ||
		previous.Section != next.Section || previous.SourceKind != next.SourceKind || previous.SlideNumber != next.SlideNumber ||
		previous.SheetName != next.SheetName || previous.RowStart != next.RowStart ||
		previous.RowEnd != next.RowEnd || previous.ColumnStart != next.ColumnStart ||
		previous.ColumnEnd != next.ColumnEnd || previous.Encoding != next.Encoding ||
		previous.HasHeader != next.HasHeader || !samePageRange(previous, next) {
		return false
	}
	return (previous.SourceEnd == 0) == (next.SourceEnd == 0)
}

func typedSectionKind(section Section) string {
	if section.Kind != "" {
		return section.Kind
	}
	return inferKind(section.Text)
}

func samePageRange(left, right Section) bool {
	leftStart, rightStart := pageNumber(left.PageNumber), pageNumber(right.PageNumber)
	leftEnd, rightEnd := left.PageEnd, right.PageEnd
	if leftEnd == 0 {
		leftEnd = leftStart
	}
	if rightEnd == 0 {
		rightEnd = rightStart
	}
	return leftStart == rightStart && leftEnd == rightEnd
}

func pageNumber(page *int) int {
	if page == nil {
		return 0
	}
	return *page
}

func normalizeChunkOptions(options ChunkOptions) ChunkOptions {
	defaultBudget := options.TokenBudget <= 0
	if options.TokenBudget <= 0 {
		options.TokenBudget = DefaultTokenBudget
	}
	if options.Overlap < 0 {
		options.Overlap = 0
	}
	if options.Overlap == 0 && defaultBudget {
		options.Overlap = DefaultOverlap
	}
	if options.Overlap >= options.TokenBudget {
		options.Overlap = options.TokenBudget / 8
	}
	if options.MaxChunks <= 0 {
		options.MaxChunks = MaxChunksPerDocument
	}
	if options.MaxChunks > MaxChunksPerDocument {
		options.MaxChunks = MaxChunksPerDocument
	}
	if options.Version == "" {
		options.Version = DefaultChunker
	}
	if options.TokenizerID == "" {
		options.TokenizerID = EstimateTokenizer
	}
	return options
}

// EstimateTokens conservatively counts CJK runes individually, ASCII word
// pieces at four characters per token, and non-space punctuation separately.
func EstimateTokens(text string) int {
	count := 0
	asciiRun := 0
	flushASCII := func() {
		if asciiRun > 0 {
			count += (asciiRun + 3) / 4
			asciiRun = 0
		}
	}
	for _, r := range text {
		if asciiTokenRune(r) {
			asciiRun++
			continue
		}
		flushASCII()
		if unicode.IsSpace(r) {
			continue
		}
		count++
	}
	flushASCII()
	return count
}

var (
	typedTextSeparators = []string{"\n\n", "\n", "。", "！", "？", "!", "?", "；", ";", "，", "、", "：", ",", ":", ".", " "}
	typedCodeSeparators = []string{"\n", "\t", ",", ";", " "}
	typedListSeparators = []string{"\n", "。", "！", "？", "；", ";", "，", "、", "：", " "}
)

func splitTextByTokens(text string, budget, overlap int, separators []string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	budget = maxInt(1, budget)
	overlap = minInt(maxInt(0, overlap), budget-1)
	runes := []rune(text)
	result := make([]string, 0)
	for start := 0; start < len(runes); {
		end := fittingTokenEnd(runes, start, budget)
		if end < len(runes) {
			minimum := fittingTokenEnd(runes, start, maxInt(1, budget/2))
			end = preferredSplitEnd(runes, start, minimum, end, separators)
		}
		if piece := strings.TrimSpace(string(runes[start:end])); piece != "" {
			result = append(result, piece)
		}
		if end == len(runes) {
			break
		}
		// Overlap is a slice of the same source. Rejoining independent strings
		// with a space used to turn e.g. 共和国 into 共 和国 at a hard split.
		next := overlapTokenStart(runes, start, end, overlap)
		if next <= start {
			next = end
		}
		start = next
	}
	return result
}

func asciiTokenRune(r rune) bool {
	return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-')
}

// Incremental counting avoids repeatedly estimating every prefix of a long
// punctuation-free paragraph. It uses exactly the EstimateTokens rules.
func fittingTokenEnd(runes []rune, start, budget int) int {
	count, asciiRun := 0, 0
	for end := start; end < len(runes); end++ {
		r := runes[end]
		cost := 0
		if asciiTokenRune(r) {
			if asciiRun%4 == 0 {
				cost = 1
			}
			asciiRun++
		} else {
			asciiRun = 0
			if !unicode.IsSpace(r) {
				cost = 1
			}
		}
		if count+cost > budget {
			return end
		}
		count += cost
	}
	return len(runes)
}

func preferredSplitEnd(runes []rune, start, minimum, end int, separators []string) int {
	for _, separator := range separators {
		pattern := []rune(separator)
		for cut := end; cut >= minimum && cut > start; cut-- {
			if cut-len(pattern) < start || string(runes[cut-len(pattern):cut]) != separator {
				continue
			}
			// A decimal point is not an English sentence boundary.
			if separator == "." && cut >= 2 && cut < len(runes) && unicode.IsDigit(runes[cut-2]) && unicode.IsDigit(runes[cut]) {
				continue
			}
			return cut
		}
	}
	return end
}

func overlapTokenStart(runes []rune, start, end, budget int) int {
	if budget <= 0 {
		return end
	}
	count, asciiRun := 0, 0
	next := end
	for next > start {
		r := runes[next-1]
		cost := 0
		if asciiTokenRune(r) {
			if asciiRun%4 == 0 {
				cost = 1
			}
			asciiRun++
		} else {
			asciiRun = 0
			if !unicode.IsSpace(r) {
				cost = 1
			}
		}
		if count+cost > budget {
			break
		}
		count += cost
		next--
	}
	// Prefer a complete clause or word within the available overlap. Without
	// such a boundary, keep the contiguous suffix, including Chinese runes.
	for i := next; i < end-1; i++ {
		if overlapBoundary(runes[i]) || strings.ContainsRune("，、：:", runes[i]) {
			return i + 1
		}
	}
	return next
}

func addHeading(heading, text string, budget int) string {
	if heading == "" {
		return strings.TrimSpace(text)
	}
	candidate := heading + "\n\n" + strings.TrimSpace(text)
	if EstimateTokens(candidate) <= budget {
		return candidate
	}
	return strings.TrimSpace(text)
}

func headingOverhead(heading string) int {
	if heading == "" {
		return 0
	}
	return 1
}

func splitLeadingHeading(text string) (string, string) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return "", text
	}
	first := strings.TrimSpace(lines[0])
	if !markdownHeadingPattern.MatchString(first) {
		return "", text
	}
	return first, strings.TrimSpace(strings.Join(lines[1:], "\n"))
}

func inferKind(text string) string {
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return KindText
	}
	if isCodeFence(lines[0]) {
		return KindCode
	}
	if len(lines) > 1 && isMarkdownTableStart(lines, 0) {
		return KindTable
	}
	if listItemPattern.MatchString(lines[0]) {
		return KindList
	}
	return KindText
}

func splitTypedTable(section Section, heading, text string, options ChunkOptions) ([]Chunk, bool) {
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return nil, false
	}
	headerCount := 0
	if section.HasHeader {
		headerCount = 1
		if len(lines) > 1 && isTableSeparator(lines[1]) {
			headerCount = 2
		}
	}
	header := strings.Join(lines[:headerCount], "\n")
	baseBudget := options.TokenBudget - EstimateTokens(heading) - headingOverhead(heading)
	if baseBudget < 1 {
		baseBudget = 1
	}
	if header != "" && EstimateTokens(header) >= baseBudget {
		return nil, true
	}

	result := make([]Chunk, 0)
	rows := make([]string, 0)
	rowStart := section.RowStart
	if rowStart <= 0 {
		rowStart = 1 + headerCount
	}
	currentStart := rowStart
	flush := func() {
		if len(rows) == 0 && header == "" {
			return
		}
		parts := make([]string, 0, len(rows)+headerCount)
		if header != "" {
			parts = append(parts, strings.Split(header, "\n")...)
		}
		parts = append(parts, rows...)
		content := strings.Join(parts, "\n")
		content = addHeading(heading, content, options.TokenBudget)
		chunk := chunkFromSection(section, KindTable, heading, content)
		chunk.RowStart = currentStart
		chunk.RowEnd = currentStart + len(rows) - 1
		if len(rows) == 0 {
			if section.RowEnd < section.RowStart {
				chunk.RowStart, chunk.RowEnd = 0, 0
			} else {
				chunk.RowEnd = chunk.RowStart
			}
		}
		result = append(result, chunk)
		currentStart += len(rows)
		rows = nil
	}

	for _, row := range lines[headerCount:] {
		candidateRows := append(append([]string(nil), rows...), row)
		candidate := strings.Join(append(append([]string(nil), strings.Split(header, "\n")...), candidateRows...), "\n")
		if heading != "" {
			candidate = heading + "\n\n" + candidate
		}
		if EstimateTokens(candidate) <= options.TokenBudget {
			rows = append(rows, row)
			continue
		}
		if len(rows) > 0 {
			flush()
		}
		rowWithHeader := row
		if header != "" {
			rowWithHeader = header + "\n" + row
		}
		if heading != "" {
			rowWithHeader = heading + "\n\n" + rowWithHeader
		}
		if EstimateTokens(rowWithHeader) <= options.TokenBudget {
			rows = append(rows, row)
			continue
		}
		bodyBudget := baseBudget - EstimateTokens(header) - 1
		if bodyBudget < 1 {
			bodyBudget = 1
		}
		for _, piece := range splitTextByTokens(row, bodyBudget, 0, tableSeparators) {
			content := piece
			if header != "" {
				content = header + "\n" + piece
			}
			content = addHeading(heading, content, options.TokenBudget)
			chunk := chunkFromSection(section, KindTable, heading, content)
			chunk.RowStart, chunk.RowEnd = currentStart, currentStart
			result = append(result, chunk)
		}
		currentStart++
	}
	flush()
	if len(result) == 0 {
		content := addHeading(heading, strings.Join(lines, "\n"), options.TokenBudget)
		result = append(result, chunkFromSection(section, KindTable, heading, content))
	}
	return result, false
}

func chunkFromSection(section Section, kind, heading, content string) Chunk {
	sourceKind := section.SourceKind
	if sourceKind == "" {
		sourceKind = SourceKindNative
	}
	chunk := Chunk{
		Content: content, SourceKind: sourceKind, PageNumber: section.PageNumber, Section: section.Section,
		Kind: kind, HeadingPath: heading, SlideNumber: section.SlideNumber,
		SheetName: section.SheetName, RowStart: section.RowStart, RowEnd: section.RowEnd,
		ColumnStart: section.ColumnStart, ColumnEnd: section.ColumnEnd,
		SourceStart: section.SourceStart, SourceEnd: section.SourceEnd,
		MergedRanges: append([]string(nil), section.MergedRanges...), Encoding: section.Encoding,
	}
	if section.PageNumber != nil && *section.PageNumber > 0 {
		chunk.PageStart, chunk.PageEnd = *section.PageNumber, *section.PageNumber
		if section.PageEnd >= chunk.PageStart {
			chunk.PageEnd = section.PageEnd
		}
	}
	return chunk
}
