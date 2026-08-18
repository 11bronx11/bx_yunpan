package grpcx

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestInjectRequestID(t *testing.T) {
	ctx := InjectRequestID(WithRequestID(context.Background(), "request-123"))
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("outgoing metadata is missing")
	}
	values := md.Get(MetadataRequestID)
	if len(values) != 1 || values[0] != "request-123" {
		t.Fatalf("outgoing request id = %v, want [request-123]", values)
	}
}

func TestInjectRequestIDLeavesContextWithoutIDUnchanged(t *testing.T) {
	ctx := InjectRequestID(context.Background())
	if _, ok := metadata.FromOutgoingContext(ctx); ok {
		t.Fatal("unexpected outgoing metadata without request id")
	}
}
