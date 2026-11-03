package admin

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

func ist(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, val.IST)
}

func live(plan string) bson.M {
	return bson.M{"name": "Class", "plan": plan, "status": "active"}
}

func TestRevenueBucketsByISTMonth(t *testing.T) {
	now := ist(2026, 10, 31, 12, 0)
	invs := []paidInvoice{
		// 00:10 IST on 1 Oct is still 30 Sep in UTC: it belongs to October.
		{ID: "a", TenantID: "t1", Plan: "pro", Billing: "monthly", Price: 24900, Amount: 24900, PaidAt: ist(2026, 10, 1, 0, 10)},
		{ID: "b", TenantID: "t2", Plan: "academy", Billing: "monthly", Price: 74900, Amount: 50000, PaidAt: ist(2026, 9, 30, 23, 50)},
	}
	r := buildRevenue(invs, map[string]bson.M{"t1": live("pro"), "t2": live("academy")}, now, 6)
	if len(r.Months) != 6 || r.Months[0].Month != "2026-05" || r.Months[5].Month != "2026-10" {
		t.Fatalf("months: %+v", r.Months)
	}
	if r.Months[5].Collected != 24900 || r.Months[5].Plans["pro"] != 24900 || r.Months[5].Invoices != 1 {
		t.Errorf("october: %+v", r.Months[5])
	}
	// Collected is the cash after credit, not the list price.
	if r.Months[4].Collected != 50000 || r.Collected != 74900 {
		t.Errorf("september %+v, total %d", r.Months[4], r.Collected)
	}
	if r.MonthlyBilling != 74900 || r.YearlyBilling != 0 {
		t.Errorf("billing split: %d %d", r.MonthlyBilling, r.YearlyBilling)
	}
}

func TestRevenueComparesWithThePeriodBefore(t *testing.T) {
	now := ist(2026, 10, 15, 12, 0)
	invs := []paidInvoice{
		{TenantID: "old", Plan: "pro", Billing: "monthly", Amount: 1000, PaidAt: ist(2025, 10, 31, 23, 0)}, // before both
		{TenantID: "old", Plan: "pro", Billing: "monthly", Amount: 2000, PaidAt: ist(2025, 11, 1, 0, 0)},   // previous period: Nov to Apr
		{TenantID: "old", Plan: "pro", Billing: "monthly", Amount: 4000, PaidAt: ist(2026, 5, 1, 0, 0)},    // this period: May to Oct
	}
	r := buildRevenue(invs, map[string]bson.M{}, now, 6)
	if r.PrevCollected != 2000 || r.Collected != 4000 {
		t.Errorf("collected %d, previous %d", r.Collected, r.PrevCollected)
	}
}

func TestRecurringSmoothsYearlyAndEndsWithTheTerm(t *testing.T) {
	now := ist(2026, 10, 15, 12, 0)
	invs := []paidInvoice{
		{TenantID: "yearly", Plan: "pro", Billing: "yearly", Price: 249900, Amount: 249900, PaidAt: ist(2026, 2, 1, 9, 0)},
		{TenantID: "lapsed", Plan: "academy", Billing: "monthly", Price: 74900, Amount: 74900, PaidAt: ist(2026, 7, 10, 9, 0)},
	}
	r := buildRevenue(invs, map[string]bson.M{"yearly": live("pro"), "lapsed": live("academy")}, now, 6)
	if want := int64(249900 / 12); r.Recurring != want || r.PayingClasses != 1 {
		t.Errorf("recurring %d (want %d), paying %d", r.Recurring, want, r.PayingClasses)
	}
	// July closes with both running; by the end of August the monthly has lapsed.
	if want := int64(249900/12 + 74900); r.Months[2].Month != "2026-07" || r.Months[2].Recurring != want {
		t.Errorf("july: %+v, want %d", r.Months[2], want)
	}
	if want := int64(249900 / 12); r.Months[3].Month != "2026-08" || r.Months[3].Recurring != want {
		t.Errorf("august: %+v, want %d", r.Months[3], want)
	}
	if r.Plans[0].Plan != "pro" || r.Plans[0].Classes != 1 || r.Plans[1].Classes != 0 {
		t.Errorf("plans: %+v", r.Plans)
	}
}

func TestRecurringCountsARenewalOnce(t *testing.T) {
	now := ist(2026, 10, 15, 12, 0)
	invs := []paidInvoice{
		{TenantID: "t", Plan: "pro", Billing: "monthly", Price: 24900, Amount: 24900, PaidAt: ist(2026, 9, 20, 9, 0)},
		{TenantID: "t", Plan: "academy", Billing: "monthly", Price: 74900, Amount: 60000, PaidAt: ist(2026, 10, 1, 9, 0)}, // upgrade mid-term
	}
	r := buildRevenue(invs, map[string]bson.M{"t": live("academy")}, now, 6)
	if r.Recurring != 74900 || r.PayingClasses != 1 {
		t.Errorf("recurring %d, paying %d", r.Recurring, r.PayingClasses)
	}
}

// Income already received stays in the report; only what recurs from now on
// drops out.
func TestRevenueKeepsHistoryOfClassesThatStopped(t *testing.T) {
	now := ist(2026, 10, 15, 12, 0)
	paid := ist(2026, 10, 2, 9, 0)
	invoice := func(tid, plan string) paidInvoice {
		return paidInvoice{ID: tid, TenantID: tid, Plan: plan, Billing: "monthly", Price: 24900, Amount: 24900, PaidAt: paid}
	}
	invs := []paidInvoice{invoice("erased", "pro"), invoice("suspended", "pro"), invoice("founder", "pro"), invoice("moved", "pro"), invoice("expired", "pro")}
	tenants := map[string]bson.M{
		"suspended": {"name": "S", "plan": "pro", "status": "suspended"},
		"founder":   {"name": "F", "plan": "pro", "status": "active", "founder": true},
		"moved":     {"name": "M", "plan": "academy", "status": "active"},
		"expired":   {"name": "E", "plan": "pro", "status": "active", "paid_until": ist(2026, 10, 10, 0, 0)},
	}
	r := buildRevenue(invs, tenants, now, 6)
	if r.Collected != 5*24900 {
		t.Errorf("collected: %d", r.Collected)
	}
	if r.Recurring != 0 || r.PayingClasses != 0 {
		t.Errorf("recurring %d, paying %d", r.Recurring, r.PayingClasses)
	}
	var erased *RevenuePayment
	for n := range r.Recent {
		if r.Recent[n].TenantID == "erased" {
			erased = &r.Recent[n]
		}
	}
	if erased == nil || erased.Class != "" {
		t.Errorf("erased class payment: %+v", erased)
	}
}

func TestRecurringLatestPaymentWins(t *testing.T) {
	// A yearly plan replaced by a monthly one that has lapsed is not still paying.
	now := ist(2026, 10, 15, 12, 0)
	invs := []paidInvoice{
		{TenantID: "t", Plan: "pro", Billing: "yearly", Price: 249900, Amount: 249900, PaidAt: ist(2026, 1, 5, 9, 0)},
		{TenantID: "t", Plan: "pro", Billing: "monthly", Price: 24900, Amount: 24900, PaidAt: ist(2026, 8, 1, 9, 0)},
	}
	r := buildRevenue(invs, map[string]bson.M{"t": live("pro")}, now, 6)
	if r.Recurring != 0 {
		t.Errorf("recurring: %d", r.Recurring)
	}
}

func TestRevenueWithNoInvoices(t *testing.T) {
	r := buildRevenue(nil, map[string]bson.M{}, ist(2026, 10, 15, 12, 0), 12)
	if len(r.Months) != 12 || r.Collected != 0 || r.Recurring != 0 || len(r.Plans) != 3 {
		t.Errorf("empty report: %+v", r)
	}
}

func TestRevenueMonthsCrossingAYearEnd(t *testing.T) {
	r := buildRevenue(nil, nil, ist(2026, 1, 31, 23, 59), 3)
	if r.Months[0].Month != "2025-11" || r.Months[2].Month != "2026-01" {
		t.Errorf("months: %s..%s", r.Months[0].Month, r.Months[2].Month)
	}
}
