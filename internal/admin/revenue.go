package admin

import (
	"context"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/billing"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

// The subscription income report. All amounts are paise and include GST, as
// the prices do. "Collected" is the cash on paid invoices, after any pro-rata
// credit, in the IST month it was paid. "Recurring" smooths yearly payments:
// the list price of each class's paid-up subscription, yearly counted at 1/12.

type RevenueMonth struct {
	Month     string           `json:"month"` // 2026-10
	Collected int64            `json:"collected"`
	Plans     map[string]int64 `json:"plans"`
	Invoices  int              `json:"invoices"`
	Recurring int64            `json:"recurring"`
}

type RevenuePlan struct {
	Plan     string `json:"plan"`
	Name     string `json:"name"`
	Amount   int64  `json:"amount"`
	Invoices int    `json:"invoices"`
	Classes  int    `json:"classes"` // paid up now
}

type RevenuePayment struct {
	ID       string    `json:"id"`
	TenantID string    `json:"tenant_id"`
	Class    string    `json:"class"` // empty once the class has been erased
	Plan     string    `json:"plan"`
	Billing  string    `json:"billing"`
	Amount   int64     `json:"amount"`
	GST      string    `json:"gst_invoice_no"`
	PaidAt   time.Time `json:"paid_at"`
}

type RevenueReport struct {
	Months         []RevenueMonth   `json:"months"`
	Collected      int64            `json:"collected"`
	PrevCollected  int64            `json:"prev_collected"` // the equal period before
	Recurring      int64            `json:"recurring"`
	PrevRecurring  int64            `json:"prev_recurring"` // at the end of last month
	PayingClasses  int              `json:"paying_classes"`
	Plans          []RevenuePlan    `json:"plans"`
	MonthlyBilling int64            `json:"monthly_billing"`
	YearlyBilling  int64            `json:"yearly_billing"`
	Recent         []RevenuePayment `json:"recent"`
}

// paidPlans is the fixed order of paid plans; the console's chart colours
// follow it.
var paidPlans = []string{"pro", "academy", "academy_plus"}

type paidInvoice struct {
	ID, TenantID, Plan, Billing, GST string
	Price, Amount                    int64
	PaidAt                           time.Time
}

func monthKey(t time.Time) string { return t.In(val.IST).Format("2006-01") }

// istMonth is the start of the IST month offset months from t's.
func istMonth(t time.Time, offset int) time.Time {
	l := t.In(val.IST)
	return time.Date(l.Year(), l.Month()+time.Month(offset), 1, 0, 0, 0, 0, val.IST)
}

func monthlyValue(i paidInvoice) int64 {
	if i.Billing == "yearly" {
		return i.Price / 12
	}
	return i.Price
}

// covering returns, per class, its latest payment at t if that is still
// running. Invoices must be sorted by PaidAt.
func covering(invs []paidInvoice, t time.Time) map[string]paidInvoice {
	out := map[string]paidInvoice{}
	for _, i := range invs {
		if i.PaidAt.After(t) {
			break
		}
		out[i.TenantID] = i
	}
	for id, i := range out {
		if !t.Before(i.PaidAt.Add(billing.Term(i.Billing))) {
			delete(out, id)
		}
	}
	return out
}

// payingNow is true when the class still has this subscription today. History
// stays as it was, but an erased, suspended or founder class, or one staff
// moved to another plan, is not recurring income any more.
func payingNow(i paidInvoice, t bson.M, now time.Time) bool {
	if t == nil || val.Str(t["status"]) == "suspended" || val.Bool(t["founder"]) || val.Str(t["plan"]) != i.Plan {
		return false
	}
	pu, ok := val.Time(t["paid_until"])
	return !ok || pu.After(now)
}

func sumRecurring(active map[string]paidInvoice) int64 {
	var n int64
	for _, i := range active {
		n += monthlyValue(i)
	}
	return n
}

// buildRevenue makes the report for the months up to and including now's.
// Invoices must be sorted by PaidAt and reach back at least 13 months and one
// whole period before the window, so recurring and the comparison are right.
func buildRevenue(invs []paidInvoice, tenants map[string]bson.M, now time.Time, months int) RevenueReport {
	r := RevenueReport{Months: make([]RevenueMonth, months), Plans: []RevenuePlan{}, Recent: []RevenuePayment{}}
	index := map[string]int{}
	for n := range months {
		start := istMonth(now, n-months+1)
		key := monthKey(start)
		index[key] = n
		end := istMonth(start, 1)
		if end.After(now) {
			end = now
		}
		// A month's recurring is what was running as it closed.
		r.Months[n] = RevenueMonth{Month: key, Plans: map[string]int64{}, Recurring: sumRecurring(covering(invs, end.Add(-time.Nanosecond)))}
	}
	if months > 1 {
		r.PrevRecurring = r.Months[months-2].Recurring
	}

	byPlan := map[string]*RevenuePlan{}
	for _, id := range paidPlans {
		byPlan[id] = &RevenuePlan{Plan: id, Name: plans.Plans[id].Name}
	}
	active := covering(invs, now)
	for id, i := range active {
		if !payingNow(i, tenants[id], now) {
			delete(active, id)
			continue
		}
		r.PayingClasses++
		if p := byPlan[i.Plan]; p != nil {
			p.Classes++
		}
	}
	r.Recurring = sumRecurring(active)
	r.Months[months-1].Recurring = r.Recurring

	windowStart, prevStart := istMonth(now, 1-months), istMonth(now, 1-2*months)
	for _, i := range invs {
		if i.PaidAt.Before(prevStart) || i.PaidAt.After(now) {
			continue
		}
		if i.PaidAt.Before(windowStart) {
			r.PrevCollected += i.Amount
			continue
		}
		m := &r.Months[index[monthKey(i.PaidAt)]]
		m.Collected += i.Amount
		m.Plans[i.Plan] += i.Amount
		m.Invoices++
		r.Collected += i.Amount
		if i.Billing == "yearly" {
			r.YearlyBilling += i.Amount
		} else {
			r.MonthlyBilling += i.Amount
		}
		if p := byPlan[i.Plan]; p != nil {
			p.Amount += i.Amount
			p.Invoices++
		}
		r.Recent = append(r.Recent, RevenuePayment{
			ID: i.ID, TenantID: i.TenantID, Class: val.Str(tenants[i.TenantID]["name"]), Plan: i.Plan, Billing: i.Billing,
			Amount: i.Amount, GST: i.GST, PaidAt: i.PaidAt,
		})
	}
	for _, id := range paidPlans {
		r.Plans = append(r.Plans, *byPlan[id])
	}
	sort.Slice(r.Recent, func(a, b int) bool { return r.Recent[a].PaidAt.After(r.Recent[b].PaidAt) })
	if len(r.Recent) > 15 {
		r.Recent = r.Recent[:15]
	}
	return r
}

func (s *Service) loadPaidInvoices(ctx context.Context, from time.Time) ([]paidInvoice, map[string]bson.M, error) {
	cur, err := s.M.AdminDB.Collection("invoices").Find(ctx, bson.M{"status": "paid", "paid_at": bson.M{"$gte": from}},
		options.Find().SetSort(bson.D{{Key: "paid_at", Value: 1}}).
			SetProjection(bson.M{"tenant_id": 1, "plan": 1, "billing": 1, "price": 1, "amount": 1, "gst_invoice_no": 1, "paid_at": 1}))
	if err != nil {
		return nil, nil, err
	}
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, nil, err
	}
	invs := make([]paidInvoice, 0, len(docs))
	seen := map[string]bool{}
	tids := []string{}
	for _, d := range docs {
		at, ok := val.Time(d["paid_at"])
		if !ok {
			continue
		}
		tid := val.Str(d["tenant_id"])
		if !seen[tid] {
			seen[tid] = true
			tids = append(tids, tid)
		}
		invs = append(invs, paidInvoice{
			ID: val.Str(d["_id"]), TenantID: tid, Plan: val.Str(d["plan"]), Billing: val.Str(d["billing"]), GST: val.Str(d["gst_invoice_no"]),
			Price: val.IntOr(d["price"], 0), Amount: val.IntOr(d["amount"], 0), PaidAt: at,
		})
	}
	cur, err = s.M.AdminDB.Collection("tenants").Find(ctx, bson.M{"_id": bson.M{"$in": tids}}, options.Find().SetProjection(tenantFields))
	if err != nil {
		return nil, nil, err
	}
	var ts []bson.M
	if err := cur.All(ctx, &ts); err != nil {
		return nil, nil, err
	}
	tenants := make(map[string]bson.M, len(ts))
	for _, t := range ts {
		tenants[val.Str(t["_id"])] = t
	}
	return invs, tenants, nil
}

func (s *Service) revenue(w http.ResponseWriter, r *http.Request) error {
	months := 12
	switch r.URL.Query().Get("months") {
	case "6":
		months = 6
	case "24":
		months = 24
	}
	now := time.Now()
	invs, tenants, err := s.loadPaidInvoices(r.Context(), istMonth(now, 1-max(2*months, 14)))
	if err != nil {
		return err
	}
	s.log(r, "revenue_viewed", "", bson.M{"months": months})
	httpx.JSON(w, 200, buildRevenue(invs, tenants, now, months))
	return nil
}
