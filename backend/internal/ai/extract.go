package ai

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	pdf "github.com/ledongthuc/pdf"
)

var (
	errUnsupported  = errors.New("unsupported file type")
	errInvalidInput = errors.New("invalid file content")
)

const (
	maxChunksPerDocument  = 256
	maxExtractedTextBytes = 4 << 20
	maxVisionImageBytes   = 10 << 20
	maxPDFPages           = 1000
)

type extractedSection struct {
	Text       string
	PageNumber *int
	Section    string
}

type chunkInput struct {
	Content    string
	PageNumber *int
	Section    string
}

func extract(ctx context.Context, provider Provider, mimeType string, data []byte) ([]extractedSection, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	mimeType = strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch mimeType {
	case "text/plain", "text/markdown", "text/x-markdown", "application/json", "text/csv":
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("%w: text is not UTF-8", errInvalidInput)
		}
		return []extractedSection{{Text: string(data)}}, nil
	case "application/pdf":
		return extractPDF(data)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return extractDOCX(data)
	case "image/jpeg", "image/png":
		if len(data) > maxVisionImageBytes {
			return nil, fmt.Errorf("%w: image exceeds AI size limit", errUnsupported)
		}
		text, err := provider.ExtractImage(ctx, mimeType, data)
		if err != nil {
			return nil, err
		}
		return []extractedSection{{Text: text, Section: "image OCR and visual description"}}, nil
	default:
		return nil, errUnsupported
	}
}

func extractPDF(data []byte) ([]extractedSection, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: open PDF: %v", errInvalidInput, err)
	}
	pages := reader.NumPage()
	if pages <= 0 || pages > maxPDFPages {
		return nil, fmt.Errorf("%w: invalid PDF page count", errInvalidInput)
	}
	fonts := make(map[string]*pdf.Font)
	sections := make([]extractedSection, 0, pages)
	totalBytes := 0
	for pageNumber := 1; pageNumber <= pages; pageNumber++ {
		page := reader.Page(pageNumber)
		for _, name := range page.Fonts() {
			if _, exists := fonts[name]; !exists {
				font := page.Font(name)
				fonts[name] = &font
			}
		}
		text, err := page.GetPlainText(fonts)
		if err != nil {
			return nil, fmt.Errorf("%w: extract PDF page %d: %v", errInvalidInput, pageNumber, err)
		}
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			continue
		}
		totalBytes += len(text)
		if totalBytes > maxExtractedTextBytes {
			return nil, fmt.Errorf("%w: PDF extracted text is too large", errInvalidInput)
		}
		pageValue := pageNumber
		sections = append(sections, extractedSection{Text: text, PageNumber: &pageValue, Section: "PDF text layer"})
	}
	if len(sections) == 0 {
		return nil, fmt.Errorf("%w: PDF has no extractable text", errInvalidInput)
	}
	return sections, nil
}

func extractDOCX(data []byte) ([]extractedSection, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, file := range archive.File {
		if file.Name != "word/document.xml" {
			continue
		}
		if file.UncompressedSize64 > maxExtractedTextBytes {
			return nil, fmt.Errorf("%w: DOCX document XML is too large", errInvalidInput)
		}
		stream, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: open DOCX XML: %v", errInvalidInput, err)
		}
		defer func() { _ = stream.Close() }()
		decoder := xml.NewDecoder(io.LimitReader(stream, maxExtractedTextBytes+1))
		var text strings.Builder
		for {
			token, err := decoder.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%w: decode DOCX XML: %v", errInvalidInput, err)
			}
			start, ok := token.(xml.StartElement)
			if !ok {
				continue
			}
			switch start.Name.Local {
			case "t":
				var value string
				if err := decoder.DecodeElement(&value, &start); err != nil {
					return nil, fmt.Errorf("%w: decode DOCX text: %v", errInvalidInput, err)
				}
				text.WriteString(value)
				text.WriteByte(' ')
			case "p":
				text.WriteByte('\n')
			}
			if text.Len() > maxExtractedTextBytes {
				return nil, fmt.Errorf("%w: DOCX extracted text is too large", errInvalidInput)
			}
		}
		if strings.TrimSpace(text.String()) == "" {
			return nil, fmt.Errorf("%w: DOCX has no extractable text", errInvalidInput)
		}
		return []extractedSection{{Text: text.String()}}, nil
	}
	return nil, fmt.Errorf("%w: DOCX document.xml not found", errInvalidInput)
}

func makeChunks(sections []extractedSection) []chunkInput {
	const size = 1200
	const overlap = 160
	result := make([]chunkInput, 0)
	for _, section := range sections {
		clean := strings.Join(strings.Fields(section.Text), " ")
		runes := []rune(clean)
		for start := 0; start < len(runes); {
			if len(result) == maxChunksPerDocument {
				return result
			}
			end := start + size
			if end > len(runes) {
				end = len(runes)
			}
			result = append(result, chunkInput{
				Content: string(runes[start:end]), PageNumber: section.PageNumber, Section: section.Section,
			})
			if end == len(runes) {
				break
			}
			start = end - overlap
		}
	}
	return result
}
