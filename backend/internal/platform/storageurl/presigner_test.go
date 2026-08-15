package storageurl

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPresignedGetObjectAddsPublicPathPrefix(t *testing.T) {
	presigner, err := New("127.0.0.1:3000", "access", "secret", "us-east-1", false, "/storage")
	if err != nil {
		t.Fatal(err)
	}

	signed, err := presigner.PresignedGetObject(context.Background(), "yunpan", "objects/example.txt", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if signed.Host != "127.0.0.1:3000" {
		t.Fatalf("unexpected public host: %s", signed.Host)
	}
	if signed.Path != "/storage/yunpan/objects/example.txt" {
		t.Fatalf("unexpected public path: %s", signed.Path)
	}
	if !strings.Contains(signed.RawQuery, "X-Amz-Signature=") {
		t.Fatal("missing signature")
	}
}
