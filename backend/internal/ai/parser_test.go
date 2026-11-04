package ai

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"testing"

	"github.com/google/uuid"
)

func TestBuiltinParserClassifiesUnsupportedAndReportsIdentity(t *testing.T) {
	parser := NewBuiltinParser(&fakeProvider{dimension: 4})
	_, err := parser.Parse(context.Background(), ParseRequest{ObjectID: uuid.New(), MediaType: "application/zip", ObjectAddress: ObjectReadAddress{Data: []byte("x")}})
	if !IsParseUnsupported(err) {
		t.Fatalf("error = %v, want unsupported parse error", err)
	}

	result, err := parser.Parse(context.Background(), ParseRequest{ObjectID: uuid.New(), MediaType: "text/plain", ObjectAddress: ObjectReadAddress{Data: []byte("hello world")}, ParserProfileVersion: "builtin-test-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ParserID != "builtin" || result.ParserVersion != "builtin-test-v1" || result.ArtifactHash == "" || result.BlockCount != 1 || result.Quality.TextBytes == 0 {
		t.Fatalf("unexpected parse result: %#v", result)
	}
}

func TestBuiltinParserContextCancellationIsRetryable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	parser := NewBuiltinParser(&fakeProvider{dimension: 4})
	_, err := parser.Parse(ctx, ParseRequest{ObjectID: uuid.New(), MediaType: "text/plain", ObjectAddress: ObjectReadAddress{Data: []byte("hello")}})
	if !IsParseRetryable(err) && !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want retryable cancellation", err)
	}
}

func TestFallbackParserFallsBackOnlyForUnsupportedFormats(t *testing.T) {
	primary := &parserStub{err: &ParseError{Code: "ai.parse.unsupported", Unsupported: true}}
	fallback := &parserStub{result: ParseResult{ParserID: "builtin"}}
	result, err := NewFallbackParser(primary, fallback).Parse(context.Background(), ParseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ParserID != "builtin" || len(result.Warnings) != 1 || primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("unexpected fallback result/calls: result=%#v primary=%d fallback=%d", result, primary.calls, fallback.calls)
	}

	primary = &parserStub{err: &ParseError{Code: "ai.parse.failed", Retryable: true}}
	fallback = &parserStub{result: ParseResult{ParserID: "builtin"}}
	_, err = NewFallbackParser(primary, fallback).Parse(context.Background(), ParseRequest{})
	if !IsParseRetryable(err) || primary.calls != 1 || fallback.calls != 0 {
		t.Fatalf("transient primary failure should be returned without fallback: err=%v primary=%d fallback=%d", err, primary.calls, fallback.calls)
	}
}

func TestChunksForIndexPrefersParserChunksAndRejectsTruncation(t *testing.T) {
	parserChunk := chunkInput{Content: "parser-produced chunk", Kind: "table", SheetName: "Revenue", RowStart: 2, RowEnd: 4}
	chunks, err := chunksForIndex(ParseResult{
		Sections: []Section{{Text: "legacy section that must not be re-chunked"}},
		Chunks:   []chunkInput{parserChunk},
	})
	if err != nil || len(chunks) != 1 || chunks[0].Content != parserChunk.Content ||
		chunks[0].Kind != parserChunk.Kind || chunks[0].SheetName != parserChunk.SheetName ||
		chunks[0].RowStart != parserChunk.RowStart || chunks[0].RowEnd != parserChunk.RowEnd {
		t.Fatalf("chunksForIndex = %#v, %v; want parser chunk with source metadata", chunks, err)
	}

	if _, err := chunksForIndex(ParseResult{ChunksTruncated: true}); !IsParsePermanent(err) {
		t.Fatalf("truncated parser result error = %v, want permanent failure", err)
	}
}

func TestChunksForIndexKeepsLegacyResponseCompatibility(t *testing.T) {
	chunks, err := chunksForIndex(ParseResult{Sections: []Section{{Text: "legacy parser output"}}})
	if err != nil || len(chunks) != 1 || chunks[0].Content != "legacy parser output" {
		t.Fatalf("legacy chunksForIndex = %#v, %v", chunks, err)
	}
}

func TestIndexerRoutesImagesToMockVisionProviderAndMarksSource(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	provider := &visionMockProvider{fakeProvider: &fakeProvider{dimension: 4}, result: "mocked visual summary"}
	parser := &parserStub{result: ParseResult{ParserID: "parser-must-not-run"}}
	service := &Service{provider: provider}
	result, err := service.parseForIndex(context.Background(), parser, ParseRequest{
		MediaType: "image/png", ObjectAddress: ObjectReadAddress{MediaType: "image/png", Data: encoded.Bytes()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if parser.calls != 0 || provider.calls != 1 {
		t.Fatalf("image routing called parser %d times and vision provider %d times", parser.calls, provider.calls)
	}
	if result.VisionModelVersion != "local-deterministic-v1" || len(result.Sections) != 1 || result.Sections[0].Kind != "vision_summary" || result.Sections[0].SourceKind != "cloud_vision" ||
		len(result.Chunks) != 1 || result.Chunks[0].Kind != "vision_summary" || result.Chunks[0].SourceKind != "cloud_vision" {
		t.Fatalf("vision provenance was not preserved: sections=%#v chunks=%#v", result.Sections, result.Chunks)
	}
}

func TestIndexerClassifiesVisionFailures(t *testing.T) {
	validPNG := func() []byte {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
			t.Fatal(err)
		}
		return encoded.Bytes()
	}
	tests := []struct {
		name      string
		data      []byte
		provider  *visionMockProvider
		permanent bool
		retryable bool
		resource  bool
	}{
		{name: "provider error", data: validPNG(), provider: &visionMockProvider{fakeProvider: &fakeProvider{dimension: 4}, err: errors.New("temporary provider outage")}, retryable: true},
		{name: "empty result", data: validPNG(), provider: &visionMockProvider{fakeProvider: &fakeProvider{dimension: 4}}, permanent: true},
		{name: "invalid image", data: []byte("not png"), provider: &visionMockProvider{fakeProvider: &fakeProvider{dimension: 4}}, permanent: true},
		{name: "oversized image", data: make([]byte, maxVisionImageBytes+1), provider: &visionMockProvider{fakeProvider: &fakeProvider{dimension: 4}}, resource: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (&Service{provider: test.provider}).parseForIndex(context.Background(), &parserStub{}, ParseRequest{
				MediaType: "image/png", ObjectAddress: ObjectReadAddress{Data: test.data},
			})
			if err == nil || IsParsePermanent(err) != test.permanent || IsParseRetryable(err) != test.retryable || IsParseResourceLimit(err) != test.resource {
				t.Fatalf("vision error classification: err=%v permanent=%t retryable=%t resource=%t", err, IsParsePermanent(err), IsParseRetryable(err), IsParseResourceLimit(err))
			}
		})
	}
}

type parserStub struct {
	result ParseResult
	err    error
	calls  int
}

type visionMockProvider struct {
	*fakeProvider
	result string
	err    error
	calls  int
}

func (p *visionMockProvider) ExtractImage(context.Context, string, []byte) (string, error) {
	p.calls++
	return p.result, p.err
}

func (s *parserStub) Parse(context.Context, ParseRequest) (ParseResult, error) {
	s.calls++
	return s.result, s.err
}
