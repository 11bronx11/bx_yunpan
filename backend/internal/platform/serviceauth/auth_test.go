package serviceauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// A05: transport identity is independently enforced for every business method.
func TestServiceBearerBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, method, configured string
		headers                  []string
		allowed                  bool
	}{
		{"valid", "/core.v1.Read/Get", "secret", []string{"Bearer secret"}, true},
		{"missing", "/core.v1.Read/Get", "secret", nil, false},
		{"wrong", "/core.v1.Read/Get", "secret", []string{"Bearer other"}, false},
		{"duplicate", "/core.v1.Read/Get", "secret", []string{"Bearer secret", "Bearer secret"}, false},
		{"prefix", "/other.v1.Read/Get", "secret", []string{"Bearer secret"}, false},
		{"case", "/core.v1.Read/Get", "secret", []string{"bearer secret"}, false},
		{"empty-config", "/core.v1.Read/Get", "", []string{"Bearer "}, false},
		{"public-health", "/grpc.health.v1.Health/Check", "secret", nil, true},
		{"health-lookalike", "/grpc.health.v1.HealthEvil/Check", "secret", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{"authorization": tc.headers})
			calls := 0
			_, err := UnaryServer(tc.configured, "/core.v1.Read/")(ctx, nil, &grpc.UnaryServerInfo{FullMethod: tc.method}, func(context.Context, any) (any, error) { calls++; return nil, nil })
			if tc.allowed && (err != nil || calls != 1) || !tc.allowed && (status.Code(err) != codes.Unauthenticated || calls != 0) {
				t.Fatalf("allowed=%v calls=%d code=%s", tc.allowed, calls, status.Code(err))
			}
		})
	}
}

// A06: signatures, audience, expiry, and the bounded token parser all fail closed.
func TestGrantNegativeMatrix(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	token, err := Sign(key, "core-retrieval-v1", time.Now().Add(time.Minute).Unix(), map[string]string{"principal": "owner-a"})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	badBody := base64.RawURLEncoding.EncodeToString([]byte(`{"aud":"core-retrieval-v1","exp":9999999999,"body":{"principal":"owner-b"}}`)) + "." + parts[1]
	makeToken := func(expires int64, raw json.RawMessage) string {
		b, _ := json.Marshal(envelope{"core-retrieval-v1", expires, raw})
		return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, b))
	}
	for _, tc := range []struct {
		name, token, audience string
		key                   ed25519.PublicKey
	}{
		{"principal-tamper", badBody, "core-retrieval-v1", pub},
		{"wrong-key", token, "core-retrieval-v1", other},
		{"cross-processing", token, "core-processing-v1", pub},
		{"expired", makeToken(time.Now().Add(-time.Second).Unix(), json.RawMessage(`{}`)), "core-retrieval-v1", pub},
		{"expiry-equal-now", makeToken(time.Now().Unix(), json.RawMessage(`{}`)), "core-retrieval-v1", pub},
		{"wrong-body", makeToken(time.Now().Add(time.Minute).Unix(), json.RawMessage(`[]`)), "core-retrieval-v1", pub},
		{"empty", "", "core-retrieval-v1", pub},
		{"malformed", "!.!", "core-retrieval-v1", pub},
		{"extra-part", token + ".extra", "core-retrieval-v1", pub},
		{"oversized", strings.Repeat("a", 32769), "core-retrieval-v1", pub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out map[string]string
			if Verify(tc.key, tc.token, tc.audience, &out) == nil {
				t.Fatal("invalid grant accepted")
			}
		})
	}
	var out map[string]string
	if Verify(pub, token, "core-retrieval-v1", &out) != nil || out["principal"] != "owner-a" {
		t.Fatal("valid grant rejected")
	}
}
