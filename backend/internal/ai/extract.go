package ai

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	pdf "github.com/ledongthuc/pdf"
	"golang.org/x/text/encoding/simplifiedchinese"
)

var (
	errUnsupported  = errors.New("unsupported file type")
	errInvalidInput = errors.New("invalid file content")
)

const (
	maxChunksPerDocument  = 256
	maxExtractedTextBytes = 4 << 20
	maxVisionImageBytes   = 10 << 20
	maxVisionSourcePixels = 40_000_000
	maxVisionEdge         = 1600
	visionReencodeBytes   = 512 << 10
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
	case "text/plain", "text/markdown", "text/x-markdown", "application/json", "text/csv",
		"text/x-c", "text/x-c++", "text/x-go", "text/x-java-source", "text/x-python",
		"application/javascript", "text/javascript", "application/typescript", "text/typescript",
		"application/sql", "application/xml", "text/xml", "application/x-sh":
		text, err := decodeText(data)
		if err != nil {
			return nil, fmt.Errorf("%w: text is not valid UTF-8 or GB18030", errInvalidInput)
		}
		return []extractedSection{{Text: text}}, nil
	case "application/pdf":
		return extractPDF(data)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return extractDOCX(data)
	case "image/jpeg", "image/png":
		if len(data) > maxVisionImageBytes {
			return nil, fmt.Errorf("%w: image exceeds AI size limit", errUnsupported)
		}
		optimizedMimeType, optimizedData, err := prepareVisionImage(mimeType, data)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalidInput, err)
		}
		text, err := provider.ExtractImage(ctx, optimizedMimeType, optimizedData)
		if err != nil {
			return nil, err
		}
		return []extractedSection{{Text: text, Section: "image OCR and visual description"}}, nil
	default:
		return nil, errUnsupported
	}
}

func decodeText(data []byte) (string, error) {
	if utf8.Valid(data) {
		if !isLikelyText(data) {
			return "", errInvalidInput
		}
		return string(data), nil
	}
	decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
	if err != nil || !utf8.Valid(decoded) || !isLikelyText(decoded) {
		return "", errInvalidInput
	}
	roundTrip, err := simplifiedchinese.GB18030.NewEncoder().Bytes(decoded)
	if err != nil || !bytes.Equal(roundTrip, data) {
		return "", errInvalidInput
	}
	return string(decoded), nil
}

func isLikelyText(data []byte) bool {
	for _, value := range string(data) {
		if value == 0 || (value < 0x20 && value != '\n' && value != '\r' && value != '\t') {
			return false
		}
	}
	return true
}

func isSourceTextFileName(name string) bool {
	base := strings.ToLower(strings.TrimSpace(filepath.Base(name)))
	switch base {
	case "dockerfile", "makefile", "cmakelists.txt", "go.mod", "go.sum", "package.json", "package-lock.json":
		return true
	}
	switch filepath.Ext(base) {
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx",
		".go", ".java", ".kt", ".kts", ".py", ".rb", ".php", ".rs", ".swift",
		".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".vue", ".svelte",
		".sh", ".bash", ".zsh", ".fish", ".ps1", ".sql", ".graphql",
		".html", ".htm", ".css", ".scss", ".less", ".xml",
		".yaml", ".yml", ".toml", ".ini", ".conf", ".properties", ".env":
		return true
	default:
		return false
	}
}

func prepareVisionImage(mimeType string, data []byte) (string, []byte, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", nil, errors.New("decode image metadata")
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxVisionSourcePixels {
		return "", nil, errors.New("invalid image dimensions")
	}
	if config.Width <= maxVisionEdge && config.Height <= maxVisionEdge && len(data) <= visionReencodeBytes {
		return mimeType, data, nil
	}

	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", nil, errors.New("decode image")
	}
	width, height := scaledDimensions(config.Width, config.Height, maxVisionEdge)
	optimized := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := source.Bounds()
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/width
			sourceY := bounds.Min.Y + y*bounds.Dy()/height
			r, g, b, a := source.At(sourceX, sourceY).RGBA()
			optimized.SetRGBA(x, y, color.RGBA{
				R: uint8((r + 0xffff - a) >> 8),
				G: uint8((g + 0xffff - a) >> 8),
				B: uint8((b + 0xffff - a) >> 8),
				A: 0xff,
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, optimized, &jpeg.Options{Quality: 82}); err != nil {
		return "", nil, errors.New("encode optimized image")
	}
	return "image/jpeg", encoded.Bytes(), nil
}

func scaledDimensions(width, height, maxEdge int) (int, int) {
	if width <= maxEdge && height <= maxEdge {
		return width, height
	}
	if width >= height {
		return maxEdge, maxInt(1, height*maxEdge/width)
	}
	return maxInt(1, width*maxEdge/height), maxEdge
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
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
