// Package fieldcrypt protects guardian phone numbers and children's dates of
// birth: AES-256-GCM ciphertext plus an HMAC blind index for phone lookups.
// The format matches what the Next.js server wrote ("v1.iv.ct.tag", base64url),
// so existing documents stay readable.
package fieldcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
)

var b64 = base64.RawURLEncoding

type Box struct{ key []byte }

func New(secret string) *Box {
	k := sha256.Sum256([]byte(secret))
	return &Box{key: k[:]}
}

func (b *Box) Encrypt(plain string) string {
	block, _ := aes.NewCipher(b.key)
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	sealed := gcm.Seal(nil, iv, []byte(plain), nil)
	ct, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	return "v1." + b64.EncodeToString(iv) + "." + b64.EncodeToString(ct) + "." + b64.EncodeToString(tag)
}

func (b *Box) Decrypt(enc string) (string, error) {
	p := strings.Split(enc, ".")
	if len(p) != 4 || p[0] != "v1" {
		return "", errors.New("unknown field encryption version")
	}
	iv, e1 := b64.DecodeString(p[1])
	ct, e2 := b64.DecodeString(p[2])
	tag, e3 := b64.DecodeString(p[3])
	if e1 != nil || e2 != nil || e3 != nil {
		return "", errors.New("bad ciphertext")
	}
	block, _ := aes.NewCipher(b.key)
	gcm, _ := cipher.NewGCM(block)
	out, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// DecryptOr returns the plain value, or "" when it cannot be read.
func (b *Box) DecryptOr(v any) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	out, err := b.Decrypt(s)
	if err != nil {
		return ""
	}
	return out
}

func (b *Box) BlindIndex(value string) string {
	m := hmac.New(sha256.New, b.key)
	m.Write([]byte(strings.TrimSpace(value)))
	return b64.EncodeToString(m.Sum(nil))
}

var (
	nonPhone = regexp.MustCompile(`[^\d+]`)
	inPlus   = regexp.MustCompile(`^\+91\d{10}$`)
	in10     = regexp.MustCompile(`^\d{10}$`)
	in91     = regexp.MustCompile(`^91\d{10}$`)
	intl     = regexp.MustCompile(`^\+\d{8,15}$`)
)

// NormalisePhone returns an E.164 number, assuming India for bare numbers.
func NormalisePhone(raw string) (string, bool) {
	d := nonPhone.ReplaceAllString(raw, "")
	switch {
	case inPlus.MatchString(d):
		return d, true
	case in10.MatchString(d):
		return "+91" + d, true
	case in91.MatchString(d):
		return "+" + d, true
	case intl.MatchString(d):
		return d, true
	}
	return "", false
}
