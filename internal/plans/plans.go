// Package plans holds the plan card, limits and the entitlements a class has
// right now. Prices include GST and are in paise. Keep the website's pricing
// in step (it reads GET /v1/plans).
package plans

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/store"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

const (
	TrialDays = 30
	GraceDays = 7
)

type Price struct {
	Monthly int64  `json:"monthly"`
	Yearly  *int64 `json:"yearly"`
}

type Limits struct {
	Students         *int64 `json:"students"`
	Batches          *int64 `json:"batches"`
	Teachers         *int64 `json:"teachers"`
	WhatsAppPerMonth int64  `json:"whatsapp_per_month"`
}

type Plan struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Tagline    string          `json:"tagline"`
	Price      *Price          `json:"price"`
	Limits     Limits          `json:"limits"`
	Features   map[string]bool `json:"features"`
	Highlights []string        `json:"highlights"`
}

func n(v int64) *int64 { return &v }

func allOn() map[string]bool {
	return map[string]bool{"reminders": true, "bulk_import": true, "ocr": true, "export": true, "late_fees": true, "statement_import": true}
}

// Plans from the Feature List and Tiers doc (24 Oct 2026).
var Plans = map[string]Plan{
	"trial": {
		ID: "trial", Name: "Free trial", Tagline: "One month, everything you need to start",
		Limits:     Limits{Students: n(20), Batches: n(3), Teachers: n(1), WhatsAppPerMonth: 0},
		Features:   map[string]bool{"reminders": false, "bulk_import": true, "ocr": true, "export": false, "late_fees": false, "statement_import": false},
		Highlights: []string{"Up to 20 students", "Ledger, cash and UPI marking, receipts", "Parent fee page", "Reminders shared from your own WhatsApp"},
	},
	"pro": {
		ID: "pro", Name: "Pro", Tagline: "For a teacher or a small class",
		Price:      &Price{Monthly: 24900, Yearly: n(249900)},
		Limits:     Limits{Students: n(50), Teachers: n(3), WhatsAppPerMonth: 400},
		Features:   allOn(),
		Highlights: []string{"Up to 50 students", "Owner and co-teachers", "Automated WhatsApp reminders, 400 a month", "PDF and Excel export, late fees"},
	},
	"academy": {
		ID: "academy", Name: "Academy", Tagline: "For institutes with several teachers",
		Price:      &Price{Monthly: 74900, Yearly: n(749900)},
		Limits:     Limits{Students: n(200), WhatsAppPerMonth: 1500},
		Features:   allOn(),
		Highlights: []string{"Up to 200 students", "Branches and teacher roles", "Automated WhatsApp reminders, 1,500 a month", "Branded parent page"},
	},
	"academy_plus": {
		ID: "academy_plus", Name: "Academy 200+", Tagline: "For large institutes",
		Price:      &Price{Monthly: 79900},
		Limits:     Limits{WhatsAppPerMonth: 2500},
		Features:   allOn(),
		Highlights: []string{"No student cap", "Automated WhatsApp reminders, 2,500 a month", "Everything in Academy"},
	},
}

var Paid = []string{"pro", "academy", "academy_plus"}

func IsPaid(id string) bool {
	for _, p := range Paid {
		if p == id {
			return true
		}
	}
	return false
}

// PriceFor returns the price for a plan and billing period, or false.
func PriceFor(plan, billing string) (int64, bool) {
	p, ok := Plans[plan]
	if !ok || p.Price == nil {
		return 0, false
	}
	if billing == "yearly" {
		if p.Price.Yearly == nil {
			return 0, false
		}
		return *p.Price.Yearly, true
	}
	return p.Price.Monthly, true
}

// State is what a class may do right now.
type State struct {
	Plan     string
	Def      Plan
	ReadOnly bool
	Features map[string]bool
}

// StateOf works out the plan state from the tenant document.
func StateOf(t bson.M, now time.Time) State {
	plan := val.Str(t["plan"])
	def, ok := Plans[plan]
	if !ok {
		plan, def = "trial", Plans["trial"]
	}
	readOnly := val.Str(t["status"]) == "suspended"
	founder := val.Bool(t["founder"])
	if plan == "trial" && !founder {
		if ends, ok := val.Date(t["trial_ends_on"]); ok {
			end, err := time.ParseInLocation("2006-01-02 15:04:05", ends+" 23:59:59", val.IST)
			if err == nil && end.Before(now) {
				readOnly = true
			}
		}
	} else if !founder {
		if pu, ok := val.Time(t["paid_until"]); ok && pu.Add(GraceDays*24*time.Hour).Before(now) {
			readOnly = true
		}
	}
	features := map[string]bool{}
	for k, v := range def.Features {
		features[k] = v
	}
	for k, v := range val.M(t["feature_flags"]) {
		if b, ok := v.(bool); ok {
			features[k] = b
		}
	}
	return State{Plan: plan, Def: def, ReadOnly: readOnly, Features: features}
}

// MonthKey names the usage month, e.g. "2026-10", in IST.
func MonthKey(t time.Time) string { return t.In(val.IST).Format("2006-01") }

// Entitlements is the block sent to the phone on every sync, with no prices
// or purchase links (Play policy). The server checks the same values.
func Entitlements(ctx context.Context, s *store.Scoped, t bson.M) (bson.M, error) {
	st := StateOf(t, time.Now())
	students, err := s.Count(ctx, "students", bson.M{"deleted": bson.M{"$ne": true}})
	if err != nil {
		return nil, err
	}
	batches, err := s.Count(ctx, "batches", bson.M{"deleted": bson.M{"$ne": true}, "archived": bson.M{"$ne": true}})
	if err != nil {
		return nil, err
	}
	usage, err := s.FindOne(ctx, "usage_counters", bson.M{"month": MonthKey(time.Now())})
	if err != nil {
		return nil, err
	}
	var trialEnds, paidUntil any
	if st.Plan == "trial" {
		trialEnds = t["trial_ends_on"]
	}
	if pu, ok := val.Time(t["paid_until"]); ok {
		paidUntil = val.Day(pu)
	}
	return bson.M{
		"plan":          st.Plan,
		"trial_ends_on": trialEnds,
		"paid_until":    paidUntil,
		"read_only":     st.ReadOnly,
		"limits": bson.M{
			"students":           st.Def.Limits.Students,
			"batches":            st.Def.Limits.Batches,
			"teachers":           st.Def.Limits.Teachers,
			"whatsapp_per_month": st.Def.Limits.WhatsAppPerMonth,
		},
		"usage":    bson.M{"students": students, "batches": batches, "whatsapp_this_month": val.IntOr(usage["whatsapp"], 0)},
		"features": st.Features,
	}, nil
}
