package ai

import (
	"context"
	"errors"
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
