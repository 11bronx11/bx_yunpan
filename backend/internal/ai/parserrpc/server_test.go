package parserrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type parserFunc func(context.Context, ai.ParseRequest) (ai.ParseResult, error)

func (f parserFunc) Parse(ctx context.Context, request ai.ParseRequest) (ai.ParseResult, error) {
	return f(ctx, request)
}

func TestServerParseDocumentCarriesSourceAndStructure(t *testing.T) {
	page := 7
	runID, objectID, versionID := uuid.New(), uuid.New(), uuid.New()
	var received ai.ParseRequest
	server := NewServer(parserFunc(func(_ context.Context, request ai.ParseRequest) (ai.ParseResult, error) {
		received = request
		return ai.ParseResult{
			ParserID: "test-parser", ParserVersion: "test-v2", PageCount: page,
			BlockCount: 1, Quality: ai.ParseQuality{TextBytes: 9, TextDensity: 0.75},
			Sections: []ai.Section{{
				Text: "row value", SourceKind: rag.SourceKindNative, PageNumber: &page, Section: "Data", Kind: "table",
				MergedRanges: []string{"A1:B1"}, Encoding: "gb18030",
			}},
			Chunks: []rag.Chunk{{
				Content: "row value", SourceKind: rag.SourceKindNative, Kind: "table", SheetName: "Sheet1", RowStart: 2, RowEnd: 2,
				MergedRanges: []string{"A1:B1"}, Encoding: "gb18030", TokenCount: 3, TokenCountIsEstimate: true,
			}},
		}, nil
	}), 1)

	response, err := server.ParseDocument(context.Background(), &pb.ParseDocumentRequest{
		RunId: runID.String(), ObjectId: objectID.String(), DocumentVersionId: versionID.String(),
		ContentHash: "sha256:test", MediaType: "text/csv", Content: []byte("name,value\na,b"),
		ParserProfileVersion: "profile-v2", SizeLimitBytes: 1024, PageLimit: 20, PixelLimit: 500,
		TraceId: "trace-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.RunID != runID || received.ObjectID != objectID || received.DocumentVersionID != versionID {
		t.Fatalf("request identities were not carried through: %#v", received)
	}
	if received.ContentHash != "sha256:test" || received.MediaType != "text/csv" || string(received.ObjectAddress.Data) != "name,value\na,b" || received.TraceID != "trace-123" {
		t.Fatalf("request content/metadata was not carried through: %#v", received)
	}
	if response.GetParserId() != "test-parser" || response.GetParserVersion() != "test-v2" || response.GetPageCount() != int32(page) || response.GetQuality().GetTextBytes() != 9 {
		t.Fatalf("unexpected parser response metadata: %#v", response)
	}
	if len(response.GetSections()) != 1 || response.GetSections()[0].GetKind() != "table" || response.GetSections()[0].GetSourceKind() != rag.SourceKindNative || response.GetSections()[0].GetPageNumber() != int32(page) {
		t.Fatalf("parsed section structure was lost: %#v", response.GetSections())
	}
	if response.GetSections()[0].GetEncoding() != "gb18030" || len(response.GetSections()[0].GetMergedRanges()) != 1 || len(response.GetChunks()) != 1 || response.GetChunks()[0].GetSourceKind() != rag.SourceKindNative || response.GetChunks()[0].GetEncoding() != "gb18030" || len(response.GetChunks()[0].GetMergedRanges()) != 1 {
		t.Fatalf("table encoding or merged-cell metadata was lost: %#v", response)
	}
	converted := ParseResultFromProto(response)
	if len(converted.Sections) != 1 || converted.Sections[0].Kind != "table" || converted.Sections[0].SourceKind != rag.SourceKindNative || converted.Sections[0].PageNumber == nil || *converted.Sections[0].PageNumber != page {
		t.Fatalf("client conversion lost section metadata: %#v", converted.Sections)
	}
	if converted.Sections[0].SourceKind != rag.SourceKindNative || converted.Sections[0].Encoding != "gb18030" || len(converted.Sections[0].MergedRanges) != 1 || len(converted.Chunks) != 1 || converted.Chunks[0].SourceKind != rag.SourceKindNative || len(converted.Chunks[0].MergedRanges) != 1 {
		t.Fatalf("client conversion lost table source metadata: %#v", converted)
	}
}

func TestServerMapsUnsupportedAndRetryableParserErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "unsupported", err: &ai.ParseError{Code: "ai.parse.unsupported", Unsupported: true}, want: codes.Unimplemented},
		{name: "retryable", err: &ai.ParseError{Code: "ai.parse.failed", Retryable: true}, want: codes.Unavailable},
		{name: "unknown", err: errors.New("internal"), want: codes.Internal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(parserFunc(func(context.Context, ai.ParseRequest) (ai.ParseResult, error) {
				return ai.ParseResult{}, test.err
			}), 1)
			_, err := server.ParseDocument(context.Background(), &pb.ParseDocumentRequest{})
			if got := status.Code(err); got != test.want {
				t.Fatalf("status code = %s, want %s (err=%v)", got, test.want, err)
			}
		})
	}
}
