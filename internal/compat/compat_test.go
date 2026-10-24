// Package compat_test checks that values written by the Next.js server
// (encrypted phones, blind indexes, signed links, admin hashes, charge ids)
// are read the same way by the Go service, so no data or sent link breaks.
// The fixtures were made with Node's crypto module.
package compat_test

import (
	"testing"

	"github.com/armohalabs/dhakshina-desk-api/internal/admin"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/fieldcrypt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/links"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/upi"
)

func TestFieldCrypto(t *testing.T) {
	b := fieldcrypt.New("k")
	got, err := b.Decrypt("v1.rkUOXkZ5VOUAwZ8K.ix0bzeBb6UkfBnUrSQ.hLWkfwy1tcY5xdht-qWTIw")
	if err != nil || got != "+919876543210" {
		t.Fatalf("decrypt node ciphertext: %q %v", got, err)
	}
	if b.BlindIndex("+919876543210") != "_Q3C10MAVOZBMnlTNe8Kfs-Q_IMGF6mXTIE9evVcUZ0" {
		t.Fatal("blind index differs from node")
	}
	if back, _ := b.Decrypt(b.Encrypt("2016-04-02")); back != "2016-04-02" {
		t.Fatal("round trip")
	}
	for in, want := range map[string]string{"98450 12345": "+919845012345", "+91-98450-12345": "+919845012345", "919845012345": "+919845012345", "+447700900123": "+447700900123", "12345": ""} {
		if got, _ := fieldcrypt.NormalisePhone(in); got != want {
			t.Fatalf("NormalisePhone(%q) = %q", in, got)
		}
	}
}

func TestSignedLinks(t *testing.T) {
	s := links.New("secret")
	tid, gid, ok := s.VerifyParent("eyJrIjoicCIsInQiOiJUMSIsImciOiJHMSIsImV4cCI6NDEwMjQ0NDgwMH0.bW4k5iOCp5zA2GXGzJw16SMHnEMAfAaN")
	if !ok || tid != "T1" || gid != "G1" {
		t.Fatal("node-signed parent link rejected")
	}
	if _, _, ok := s.VerifyUpgrade("eyJrIjoicCIsInQiOiJUMSIsImciOiJHMSIsImV4cCI6NDEwMjQ0NDgwMH0.bW4k5iOCp5zA2GXGzJw16SMHnEMAfAaN"); ok {
		t.Fatal("parent link accepted as upgrade link")
	}
	tok, _ := s.Upgrade("T", "U")
	if _, _, ok := links.New("other").VerifyUpgrade(tok); ok {
		t.Fatal("wrong secret accepted")
	}
	if tt, u, ok := s.VerifyUpgrade(tok); !ok || tt != "T" || u != "U" {
		t.Fatal("round trip")
	}
}

func TestAdminHash(t *testing.T) {
	node := "a@b.in=scrypt$saltsalt$9aEtSgW4ZTS2_IPM2RWm6KikxxkGVC-325-qekyZARs"
	if !admin.CheckPassword(node, "A@B.in", "pw-123456789") || admin.CheckPassword(node, "a@b.in", "nope") {
		t.Fatal("node scrypt hash not verified")
	}
	if !admin.CheckPassword("x@y.in="+admin.HashPassword("long password 1", admin.NewSalt()), "x@y.in", "long password 1") {
		t.Fatal("go hash round trip")
	}
}

func TestIDsAndUPI(t *testing.T) {
	if ids.CycleChargeID("E1", "2026-10") != "fb3cc8ec-2bbf-5909-9bb1-6447d1369dec" {
		t.Fatal("cycle charge id differs from node/app")
	}
	if !ids.IsUUID(ids.UUIDv7()) {
		t.Fatal("uuidv7")
	}
	if got := upi.InvoiceRef("0192ab34-0000-7000-8000-9abc1234ef00", "2026-10"); got != "DDBC1234EF00202610" {
		t.Fatalf("invoice ref %s", got)
	}
	l := upi.Link(upi.Pay{VPA: "lakshmi@okhdfc", Payee: "Lakshmi R", AmountPaise: 150050, Ref: "DDX", Note: "Ananya Bharatanatyam"})
	if l != "upi://pay?pa=lakshmi%40okhdfc&pn=Lakshmi%20R&am=1500.50&cu=INR&tr=DDX&tn=Ananya%20Bharatanatyam" {
		t.Fatalf("upi link %s", l)
	}
}
