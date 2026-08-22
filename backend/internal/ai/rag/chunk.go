package rag

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxChunksPerDocument = 256
	ChunkSize            = 1200
	ChunkOverlap         = 160
)

var headingPattern = regexp.MustCompile(`^#{1,6}\s+`)

// Section is extracted document text plus the metadata needed for citations.
// Text may contain paragraph boundaries as newlines.
type Section struct {
	Text       string
	PageNumber *int
	Section    string
}

// Chunk is the indexable unit produced from an extracted section.
type Chunk struct {
	Content    string
	PageNumber *int
	Section    string
}

// MakeChunks keeps headings and paragraph/sentence boundaries whenever the
// resulting chunk fits. A hard rune split is the final fallback for a single
// oversized sentence or paragraph. The cap prevents pathological documents
// from creating unbounded embedding work.
func MakeChunks(sections []Section) []Chunk {
	result := make([]Chunk, 0)
	for _, section := range sections {
		pending := ""
		flush := func() {
			if pending == "" || len(result) == MaxChunksPerDocument {
				return
			}
			result = append(result, Chunk{Content: pending, PageNumber: section.PageNumber, Section: section.Section})
			pending = ""
		}
		for _, unit := range structureUnits(section.Text) {
			for _, content := range splitUnit(unit) {
				if pending == "" {
					pending = content
					continue
				}
				if utf8.RuneCountInString(pending)+2+utf8.RuneCountInString(content) <= ChunkSize {
					pending += "\n\n" + content
					continue
				}
				flush()
				pending = content
			}
			if len(result) == MaxChunksPerDocument {
				return result
			}
		}
		flush()
		if len(result) == MaxChunksPerDocument {
			return result
		}
	}
	return result
}

// structureUnits returns paragraph-sized units and attaches a heading to the
// following paragraph. Newline-only input is treated as one paragraph.
func structureUnits(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	blocks := strings.Split(text, "\n\n")
	units := make([]string, 0, len(blocks))
	for index := 0; index < len(blocks); index++ {
		lines := strings.Split(blocks[index], "\n")
		cleanLines := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.Join(strings.Fields(line), " ")
			if line != "" {
				cleanLines = append(cleanLines, line)
			}
		}
		if len(cleanLines) == 0 {
			continue
		}
		block := strings.Join(cleanLines, " ")
		if isHeading(block) && index+1 < len(blocks) {
			next := strings.Join(strings.Fields(strings.ReplaceAll(blocks[index+1], "\n", " ")), " ")
			if next != "" {
				block += "\n" + next
				index++
			}
		}
		units = append(units, block)
	}
	return units
}

func isHeading(value string) bool {
	return headingPattern.MatchString(value) || (utf8.RuneCountInString(value) <= 80 && !strings.ContainsAny(value, "。！？.!?;；"))
}

func splitUnit(unit string) []string {
	unit = strings.TrimSpace(unit)
	if utf8.RuneCountInString(unit) <= ChunkSize {
		return []string{unit}
	}
	parts := splitSentences(unit)
	result := make([]string, 0, len(parts))
	current := ""
	for _, part := range parts {
		if current == "" {
			current = part
			continue
		}
		if utf8.RuneCountInString(current)+1+utf8.RuneCountInString(part) <= ChunkSize {
			current += " " + part
			continue
		}
		result = append(result, current)
		current = part
	}
	if current != "" {
		result = append(result, current)
	}
	final := make([]string, 0, len(result))
	for _, part := range result {
		if utf8.RuneCountInString(part) <= ChunkSize {
			final = append(final, part)
			continue
		}
		final = append(final, hardSplit(part)...)
	}
	return final
}

func splitSentences(value string) []string {
	result := make([]string, 0)
	start := 0
	runes := []rune(value)
	for index, value := range runes {
		if !strings.ContainsRune("。！？!?；;", value) {
			continue
		}
		end := index + 1
		if part := strings.TrimSpace(string(runes[start:end])); part != "" {
			result = append(result, part)
		}
		start = end
	}
	if part := strings.TrimSpace(string(runes[start:])); part != "" {
		result = append(result, part)
	}
	if len(result) == 0 {
		return []string{value}
	}
	return result
}

func hardSplit(value string) []string {
	runes := []rune(value)
	result := make([]string, 0, (len(runes)+ChunkSize-1)/ChunkSize)
	for start := 0; start < len(runes); {
		end := start + ChunkSize
		if end > len(runes) {
			end = len(runes)
		}
		result = append(result, string(runes[start:end]))
		if end == len(runes) {
			break
		}
		start = end - ChunkOverlap
	}
	return result
}
