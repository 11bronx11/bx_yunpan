package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

type TokenManager struct {
	issuer     string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func NewTokenManager(cfg config.Auth) *TokenManager {
	seed := sha256.Sum256([]byte(cfg.SigningSeed))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	return &TokenManager{
		issuer:     cfg.Issuer,
		privateKey: privateKey,
		publicKey:  privateKey.Public().(ed25519.PublicKey),
		accessTTL:  cfg.AccessTTL,
		refreshTTL: cfg.RefreshTTL,
	}
}

func (m *TokenManager) AccessToken(userID uuid.UUID) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(m.accessTTL)
	claims := jwt.RegisteredClaims{
		Issuer:    m.issuer,
		Subject:   userID.String(),
		Audience:  jwt.ClaimStrings{"bx-yunpan-api"},
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
		ID:        uuid.Must(uuid.NewV7()).String(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := token.SignedString(m.privateKey)
	return signed, expiresAt, err
}

func (m *TokenManager) VerifyAccess(raw string) (uuid.UUID, error) {
	claims := new(jwt.RegisteredClaims)
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodEdDSA.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		return m.publicKey, nil
	}, jwt.WithIssuer(m.issuer), jwt.WithAudience("bx-yunpan-api"), jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(claims.Subject)
}

func (m *TokenManager) RefreshToken() (string, []byte, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, time.Time{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(encoded))
	return encoded, hash[:], time.Now().UTC().Add(m.refreshTTL), nil
}

func RefreshHash(raw string) []byte {
	hash := sha256.Sum256([]byte(raw))
	return hash[:]
}
