// Package ids makes the ids the app and server share: time-ordered UUIDv7
// for records, deterministic UUIDv5 for cycle charges.
package ids

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// UUIDv7 returns a time-ordered UUID string.
func UUIDv7() string { return uuidv7At(time.Now()) }

func uuidv7At(t time.Time) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(t.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return format(b[:])
}

// UUIDv5 is the RFC 4122 name-based (SHA-1) UUID.
func UUIDv5(namespace, name string) string {
	ns, _ := hex.DecodeString(strings.ReplaceAll(namespace, "-", ""))
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name))
	b := h.Sum(nil)[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return format(b)
}

// ChargeNS is the namespace for cycle charge ids (docs/api-contract.md).
const ChargeNS = "6f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f"

// CycleChargeID is the id both the app and the server give a cycle charge.
func CycleChargeID(enrolmentID, cycle string) string {
	return UUIDv5(ChargeNS, enrolmentID+":"+cycle)
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether v is a UUID string.
func IsUUID(v any) bool {
	s, ok := v.(string)
	return ok && uuidRE.MatchString(s)
}

func format(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
