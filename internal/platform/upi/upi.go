// Package upi builds the upi://pay deep link that sends a parent's money
// straight to the teacher's own UPI ID. Dhakshina Desk never holds money.
package upi

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var vpaRE = regexp.MustCompile(`^[a-zA-Z0-9.\-_]{2,256}@[a-zA-Z][a-zA-Z0-9.\-]{1,64}$`)

func IsVPA(s string) bool { return vpaRE.MatchString(strings.TrimSpace(s)) }

type Pay struct {
	VPA         string
	Payee       string
	AmountPaise int64
	Ref         string
	Note        string
}

// Link builds upi://pay with the parameters in the order UPI apps expect.
func Link(p Pay) string {
	q := []string{
		"pa=" + esc(strings.TrimSpace(p.VPA)),
		"pn=" + esc(cut(p.Payee, 50)),
		"am=" + fmt.Sprintf("%d.%02d", p.AmountPaise/100, p.AmountPaise%100),
		"cu=INR",
		"tr=" + esc(cut(p.Ref, 35)),
		"tn=" + esc(cut(p.Note, 50)),
	}
	return "upi://pay?" + strings.Join(q, "&")
}

var nonAlnum = regexp.MustCompile(`[^0-9A-Za-z]`)

// InvoiceRef encodes the enrolment and cycle, so a payment from a reminder
// link can be matched to its batch: "DD" + last 10 hex of the id + cycle.
func InvoiceRef(enrolmentID, cycle string) string {
	h := strings.ToUpper(strings.ReplaceAll(enrolmentID, "-", ""))
	if len(h) > 10 {
		h = h[len(h)-10:]
	}
	return "DD" + h + nonAlnum.ReplaceAllString(cycle, "")
}

// esc matches JavaScript's URLSearchParams encoding with %20 for spaces.
func esc(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
