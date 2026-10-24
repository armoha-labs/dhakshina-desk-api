// Package links signs the expiring tokens in links sent on WhatsApp: the
// parent family page (/p/<token>) and the owner's upgrade checkout
// (/subscribe/<token>). Format: base64url(json) "." first 32 chars of
// base64url(hmac-sha256), the same as the Next.js server used, so links
// already sent keep working.
package links

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

const (
	ParentTTL  = 60 * 24 * time.Hour
	UpgradeTTL = 14 * 24 * time.Hour
)

var b64 = base64.RawURLEncoding

type payload struct {
	K   string `json:"k"`
	T   string `json:"t"`
	G   string `json:"g,omitempty"`
	U   string `json:"u,omitempty"`
	Exp int64  `json:"exp"`
}

type Signer struct{ secret string }

func New(secret string) *Signer { return &Signer{secret: secret} }

func (s *Signer) sig(body string) string {
	m := hmac.New(sha256.New, []byte(s.secret))
	m.Write([]byte(body))
	return b64.EncodeToString(m.Sum(nil))[:32]
}

func (s *Signer) sign(p payload) string {
	j, _ := json.Marshal(p)
	body := b64.EncodeToString(j)
	return body + "." + s.sig(body)
}

func (s *Signer) verify(token, kind string) (payload, bool) {
	var p payload
	if t, err := url.PathUnescape(token); err == nil {
		token = t
	}
	i := strings.LastIndex(token, ".")
	if i < 1 {
		return p, false
	}
	body := token[:i]
	if !hmac.Equal([]byte(token[i+1:]), []byte(s.sig(body))) {
		return p, false
	}
	j, err := b64.DecodeString(body)
	if err != nil || json.Unmarshal(j, &p) != nil {
		return p, false
	}
	if p.K != kind || p.Exp <= time.Now().Unix() {
		return p, false
	}
	return p, true
}

// Parent signs a family page token for one guardian of one class.
func (s *Signer) Parent(tenantID, guardianID string) (string, time.Time) {
	exp := time.Now().Add(ParentTTL).Truncate(time.Second)
	return s.sign(payload{K: "p", T: tenantID, G: guardianID, Exp: exp.Unix()}), exp
}

// Upgrade signs a checkout token for the class owner.
func (s *Signer) Upgrade(tenantID, userID string) (string, time.Time) {
	exp := time.Now().Add(UpgradeTTL).Truncate(time.Second)
	return s.sign(payload{K: "u", T: tenantID, U: userID, Exp: exp.Unix()}), exp
}

func (s *Signer) VerifyParent(token string) (tenantID, guardianID string, ok bool) {
	p, ok := s.verify(token, "p")
	return p.T, p.G, ok && p.T != "" && p.G != ""
}

func (s *Signer) VerifyUpgrade(token string) (tenantID, userID string, ok bool) {
	p, ok := s.verify(token, "u")
	return p.T, p.U, ok && p.T != "" && p.U != ""
}
