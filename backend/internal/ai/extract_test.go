package ai

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	textunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

func TestExtractPDFRejectsRawBytes(t *testing.T) {
	sections, err := extractPDF([]byte("%PDF-1.7 this is not a valid PDF stream"))
	if err == nil || len(sections) != 0 {
		t.Fatalf("expected invalid PDF to be rejected, got sections=%d err=%v", len(sections), err)
	}
	if !errors.Is(err, errInvalidInput) {
		t.Fatalf("expected permanent content error, got %v", err)
	}
}

func TestMakeChunksCapsDocument(t *testing.T) {
	content := strings.Repeat("x", (maxChunksPerDocument+20)*1200)
	chunks := makeChunks([]extractedSection{{Text: content}})
	if len(chunks) != maxChunksPerDocument {
		t.Fatalf("expected %d chunks, got %d", maxChunksPerDocument, len(chunks))
	}
	for _, chunk := range chunks {
		if len([]rune(chunk.Content)) > 1200 {
			t.Fatalf("chunk exceeded size limit: %d", len([]rune(chunk.Content)))
		}
	}
}

func TestExtractDOCXClassifiesOversizedDocumentXMLAsResourceLimit(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(strings.Repeat("x", maxExtractedTextBytes+1))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = extractDOCX(archive.Bytes())
	if !errors.Is(err, errResourceLimit) {
		t.Fatalf("expected oversized DOCX to be rejected as a resource limit, got %v", err)
	}
}

func TestExtractDOCXPreservesParagraphBoundaries(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	const document = `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>第一段。</w:t></w:r></w:p><w:p><w:r><w:t>第二段。</w:t></w:r></w:p></w:body></w:document>`
	if _, err := entry.Write([]byte(document)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	sections, err := extractDOCX(archive.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	normalized := ""
	if len(sections) == 1 {
		normalized = normalizeParagraphText(sections[0].Text)
	}
	if len(sections) != 1 || normalized != "第一段。\n\n第二段。" {
		t.Fatalf("expected paragraph boundary, got %#v", sections)
	}
}

func TestExtractDOCXPreservesWordsAcrossFormattingRuns(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	const document = `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>中文</w:t></w:r><w:r><w:t>搜索</w:t></w:r><w:r><w:t xml:space="preserve"> Postgre</w:t></w:r><w:r><w:t>SQL</w:t><w:tab/><w:t>词组</w:t><w:br/><w:t>下一行</w:t></w:r></w:p></w:body></w:document>`
	if _, err := entry.Write([]byte(document)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	sections, err := extractDOCX(archive.Bytes())
	if err != nil || len(sections) != 1 || strings.TrimSpace(sections[0].Text) != "中文搜索 PostgreSQL\t词组\n下一行" {
		t.Fatalf("formatting runs broke source words: %#v, %v", sections, err)
	}
}

func TestExtractRejectsOversizedVisionImage(t *testing.T) {
	provider := &fakeProvider{dimension: 1024}
	_, err := extract(context.Background(), provider, "image/jpeg", make([]byte, maxVisionImageBytes+1))
	if !errors.Is(err, errResourceLimit) {
		t.Fatalf("expected oversized image to be a resource limit, got %v", err)
	}
}

func TestPrepareVisionImageResizesAndReencodesLargeImage(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2400, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 2400; x++ {
			source.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}

	mimeType, optimized, err := prepareVisionImage("image/png", input.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if mimeType != "image/jpeg" {
		t.Fatalf("expected optimized JPEG, got %q", mimeType)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(optimized))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 1600 || config.Height != 800 {
		t.Fatalf("unexpected optimized dimensions %dx%d", config.Width, config.Height)
	}
	if len(optimized) > 2<<20 {
		t.Fatalf("optimized image is unexpectedly large: %d bytes", len(optimized))
	}
}

func TestPrepareVisionImageRejectsInvalidBytes(t *testing.T) {
	if _, _, err := prepareVisionImage("image/png", []byte("not an image")); err == nil {
		t.Fatal("expected invalid image to be rejected")
	}
}

func TestPrepareVisionImageEnforcesRequestPixelLimit(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2, 1))
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareVisionImageWithPixelLimit("image/png", input.Bytes(), 1); err == nil {
		t.Fatal("expected image above the request pixel limit to be rejected")
	}
}

func TestPixelLimitComparisonDoesNotOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if !pixelLimitExceeded(maxInt, maxInt, maxVisionSourcePixels) {
		t.Fatal("expected overflowing dimensions to exceed the pixel limit")
	}
}

func TestSourceTextFileNameRecognition(t *testing.T) {
	for _, name := range []string{"main.cpp", "handler.go", "Dockerfile", "CMakeLists.txt", "config.yaml", "App.tsx"} {
		if !isSourceTextFileName(name) {
			t.Fatalf("expected %q to be recognized as source text", name)
		}
	}
	for _, name := range []string{"archive.zip", "program.exe", "photo.png", "unknown.bin"} {
		if isSourceTextFileName(name) {
			t.Fatalf("expected %q to remain binary", name)
		}
	}
}

func TestExtractAcceptsSourceMIMEType(t *testing.T) {
	sections, err := extract(context.Background(), &fakeProvider{dimension: 1024}, "text/x-c++", []byte("int main() { return 0; }"))
	if err != nil || len(sections) != 1 {
		t.Fatalf("expected C++ source extraction, sections=%d err=%v", len(sections), err)
	}
}

func TestExtractConvertsGB18030SourceText(t *testing.T) {
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("// 输入输出\r\nint main() { return 0; }\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	sections, err := extract(context.Background(), &fakeProvider{dimension: 1024}, "text/x-c++", encoded)
	if err != nil || len(sections) != 1 || !strings.Contains(sections[0].Text, "输入输出") {
		t.Fatalf("expected decoded GB18030 source, sections=%v err=%v", sections, err)
	}
}

func TestDecodeTextRejectsBinaryControls(t *testing.T) {
	if _, err := decodeText([]byte{'a', 0, 'b'}); err == nil {
		t.Fatal("expected binary control bytes to be rejected")
	}
}

func TestDecodeTextPreservesChineseAcrossEncodings(t *testing.T) {
	const source = "中文搜索 PostgreSQL\n智能云盘。"
	for _, enc := range []encoding.Encoding{
		textunicode.UTF8BOM,
		textunicode.UTF16(textunicode.LittleEndian, textunicode.UseBOM),
		textunicode.UTF16(textunicode.BigEndian, textunicode.UseBOM),
		utf32.UTF32(utf32.LittleEndian, utf32.UseBOM),
		utf32.UTF32(utf32.BigEndian, utf32.UseBOM),
		simplifiedchinese.GB18030,
	} {
		data, err := enc.NewEncoder().Bytes([]byte(source))
		if err != nil {
			t.Fatal(err)
		}
		text, err := decodeText(data)
		if err != nil || text != source {
			t.Fatalf("encoding %v: text=%q err=%v", enc, text, err)
		}
	}
	for _, data := range [][]byte{{0xff, 0xfe, 0}, {0xff, 0xfe, 0, 0xd8}, {0, 0, 0xfe, 0xff, 0, 0x11, 0, 0}} {
		if _, err := decodeText(data); !errors.Is(err, errInvalidInput) {
			t.Fatalf("invalid Unicode accepted: %x, %v", data, err)
		}
	}
}

func TestExtractJSONMatchesStrictParserPolicy(t *testing.T) {
	legacy, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(`{"名称":"中文"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{legacy, []byte(`{"名称":}`)} {
		if _, err := extract(context.Background(), &fakeProvider{dimension: 4}, "application/json; charset=utf-8", data); !errors.Is(err, errInvalidInput) {
			t.Fatalf("invalid or legacy-encoded JSON accepted: %v", err)
		}
	}
	if _, err := extract(context.Background(), &fakeProvider{dimension: 4}, "application/json", []byte(`{"名称":"中文"}`)); err != nil {
		t.Fatal(err)
	}
}
