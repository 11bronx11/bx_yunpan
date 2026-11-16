// Package serviceauth authenticates transport peers separately from end-user
// scope and task grants. Verifiers cannot mint principal/processing grants.
package serviceauth

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var ErrGrant = errors.New("invalid internal grant")

type envelope struct {
	Audience string          `json:"aud"`
	Expires  int64           `json:"exp"`
	Body     json.RawMessage `json:"body"`
}

func Private(seed string) (ed25519.PrivateKey, error) {
	b, e := base64.RawURLEncoding.DecodeString(seed)
	if e != nil || len(b) != ed25519.SeedSize {
		return nil, ErrGrant
	}
	return ed25519.NewKeyFromSeed(b), nil
}
func Public(value string) (ed25519.PublicKey, error) {
	b, e := base64.RawURLEncoding.DecodeString(value)
	if e != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrGrant
	}
	return ed25519.PublicKey(b), nil
}
func Sign(key ed25519.PrivateKey, audience string, expires int64, v any) (string, error) {
	if len(key) != ed25519.PrivateKeySize || expires <= time.Now().Unix() {
		return "", ErrGrant
	}
	body, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	b, e := json.Marshal(envelope{audience, expires, body})
	if e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, b)), nil
}
func Verify(key ed25519.PublicKey, token, audience string, out any) error {
	if len(key) != ed25519.PublicKeySize || len(token) > 32768 {
		return ErrGrant
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return ErrGrant
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return ErrGrant
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil || !ed25519.Verify(key, b, sig) {
		return ErrGrant
	}
	var env envelope
	if json.Unmarshal(b, &env) != nil || env.Audience != audience || env.Expires <= time.Now().Unix() {
		return ErrGrant
	}
	if json.Unmarshal(env.Body, out) != nil {
		return ErrGrant
	}
	return nil
}
func UnaryServer(token string, allowedPrefix string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") {
			return next(ctx, req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		v := md.Get("authorization")
		if token == "" || len(v) != 1 || subtle.ConstantTimeCompare([]byte(v[0]), []byte("Bearer "+token)) != 1 || !strings.HasPrefix(info.FullMethod, allowedPrefix) {
			return nil, status.Error(codes.Unauthenticated, "service authentication required")
		}
		return next(ctx, req)
	}
}
func UnaryClient(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), method, req, reply, cc, opts...)
	}
}
