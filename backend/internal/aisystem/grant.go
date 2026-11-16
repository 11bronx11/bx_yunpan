package aisystem

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
)

func scopeBody(token string) (aiprotocol.ScopeGrant, error) {
	var g aiprotocol.ScopeGrant
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return g, aiprotocol.ErrInvalid
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return g, err
	}
	var env struct {
		Body json.RawMessage `json:"body"`
	}
	if err = json.Unmarshal(b, &env); err != nil {
		return g, err
	}
	err = json.Unmarshal(env.Body, &g)
	return g, err
}
