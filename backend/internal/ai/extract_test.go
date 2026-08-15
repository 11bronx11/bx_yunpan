package ai

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
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
