// Package jwt is a minimal HS256 JWT, enough for the access tokens in the
// API contract and the staff tokens of the admin console.
package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalid = errors.New("invalid token")
	ErrExpired = errors.New("token expired")
)

var b64 = base64.RawURLEncoding

// Claims holds the standard claims plus whatever the caller adds.
type Claims map[string]any

func (c Claims) Str(k string) string {
	s, _ := c[k].(string)
	return s
}

// Sign adds iat and exp and signs the claims.
func Sign(claims Claims, secret string, ttl time.Duration) string {
	now := time.Now().Unix()
	out := Claims{}
	for k, v := range claims {
		out[k] = v
	}
	out["iat"] = now
	out["exp"] = now + int64(ttl/time.Second)
	header := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(out)
	unsigned := header + "." + b64.EncodeToString(body)
	return unsigned + "." + b64.EncodeToString(mac(unsigned, secret))
}

// Verify checks the signature and expiry.
func Verify(token, secret string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrInvalid
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac(parts[0]+"."+parts[1], secret)) {
		return nil, ErrInvalid
	}
	var h struct{ Alg string }
	hb, err := b64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil || h.Alg != "HS256" {
		return nil, ErrInvalid
	}
	bb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrInvalid
	}
	var c Claims
	if json.Unmarshal(bb, &c) != nil {
		return nil, ErrInvalid
	}
	exp, ok := c["exp"].(float64)
	if !ok || int64(exp) <= time.Now().Unix() {
		return nil, ErrExpired
	}
	return c, nil
}

func mac(s, secret string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(s))
	return m.Sum(nil)
}
