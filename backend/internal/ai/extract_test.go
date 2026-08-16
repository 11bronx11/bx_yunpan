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

	"golang.org/x/text/encoding/simplifiedchinese"
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

func TestExtractDOCXRejectsOversizedDocumentXML(t *testing.T) {
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
	if !errors.Is(err, errInvalidInput) {
		t.Fatalf("expected oversized DOCX to be rejected permanently, got %v", err)
	}
}

func TestExtractRejectsOversizedVisionImage(t *testing.T) {
	provider := &fakeProvider{dimension: 1024}
	_, err := extract(context.Background(), provider, "image/jpeg", make([]byte, maxVisionImageBytes+1))
	if !errors.Is(err, errUnsupported) {
		t.Fatalf("expected oversized image to be unsupported, got %v", err)
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
