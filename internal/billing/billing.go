// Package billing is the SaaS's own subscription billing: teachers pay
// Dhakshina Desk on the web, never in the Android app (Play policy). The
// gateway webhook is what switches the plan.
package billing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/billing/gateway"
	"github.com/armohalabs/dhakshina-desk-api/internal/changelog"
	"github.com/armohalabs/dhakshina-desk-api/internal/jobs"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
	"github.com/armohalabs/dhakshina-desk-api/internal/privacy"
)

type Service struct{ *app.App }

const day = 24 * time.Hour

// Term is how long one payment covers.
func Term(billing string) time.Duration {
	if billing == "yearly" {
		return 365 * day
	}
	return 30 * day
}

// ProRataCredit is the unused part of the current paid term, credited
// against a change of plan.
func ProRataCredit(t bson.M, now time.Time) int64 {
	pu, ok := val.Time(t["paid_until"])
	last := val.IntOr(t["last_paid_amount"], 0)
	billing := val.Str(t["billing"])
	if !ok || last == 0 || billing == "" || val.Str(t["plan"]) == "trial" {
		return 0
	}
	left := pu.Sub(now)
	if left <= 0 {
		return 0
	}
	if left > Term(billing) {
		left = Term(billing)
	}
	return int64(float64(last) * float64(left) / float64(Term(billing)))
}

type Quote struct {
	Plan    string `json:"plan"`
	Billing string `json:"billing"`
	Price   int64  `json:"price"`
	Credit  int64  `json:"credit"`
	Amount  int64  `json:"amount"`
}

func QuoteFor(t bson.M, plan, billing string, now time.Time) (Quote, error) {
	if !plans.IsPaid(plan) {
		return Quote{}, httpx.Err(httpx.BadRequest, "Pick Pro or Academy.")
	}
	price, ok := plans.PriceFor(plan, billing)
	if !ok {
		return Quote{}, httpx.Err(httpx.BadRequest, plans.Plans[plan].Name+" is billed monthly.")
	}
	// Renewing the same plan extends it; changing plan credits the unused days.
	var credit int64
	if !(val.Str(t["plan"]) == plan && val.Str(t["billing"]) == billing) {
		credit = ProRataCredit(t, now)
	}
	return Quote{Plan: plan, Billing: billing, Price: price, Credit: credit, Amount: max(price-credit, 100)}, nil
}

// gateway is the one the platform admin made active.
func (s *Service) gateway(ctx context.Context) (gateway.Gateway, error) {
	set, err := gateway.Load(ctx, s.M.DB, s.Box, s.Cfg)
	if err != nil {
		return nil, err
	}
	g := set.For(s.Cfg, set.Active)
	if g == nil {
		return nil, fmt.Errorf("payment gateway %q is not available", set.Active)
	}
	return g, nil
}

func (s *Service) CreateCheckout(ctx context.Context, tenantID, userID, plan, billing string) (bson.M, error) {
	var t, user bson.M
	if err := s.M.C("tenants").FindOne(ctx, bson.M{"_id": tenantID}).Decode(&t); err != nil {
		if db.NotFound(err) {
			return nil, httpx.Err(httpx.NotFound, "Class not found.")
		}
		return nil, err
	}
	_ = s.M.C("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&user)
	q, err := QuoteFor(t, plan, billing, time.Now())
	if err != nil {
		return nil, err
	}
	gw, err := s.gateway(ctx)
	if err != nil {
		return nil, err
	}
	invoiceID := ids.UUIDv7()
	name := val.Str(user["name"])
	if name == "" {
		name = val.Str(t["owner_name"])
	}
	if name == "" {
		name = val.Str(t["name"])
	}
	orderID, client, err := gw.CreateOrder(ctx, gateway.OrderInput{
		InvoiceID: invoiceID, AmountPaise: q.Amount, CustomerID: tenantID,
		Phone: s.Box.DecryptOr(user["phone_enc"]), Name: name,
		Description: fmt.Sprintf("Dhakshina Desk %s · %s", plans.Plans[plan].Name, billing),
		ReturnURL:   s.WebURL("/subscribe/done?invoice=" + invoiceID),
		NotifyURL:   s.Cfg.APIBaseURL + "/v1/billing/webhook/" + gw.Name(),
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.M.C("invoices").InsertOne(ctx, bson.M{
		"_id": invoiceID, "tenant_id": tenantID, "user_id": userID,
		"plan": q.Plan, "billing": q.Billing, "price": q.Price, "credit": q.Credit, "amount": q.Amount,
		"status": "created", "provider": gw.Name(), "provider_order_id": orderID, "created_at": time.Now(),
	}); err != nil {
		return nil, err
	}
	return bson.M{"invoice_id": invoiceID, "quote": q, "client": client}, nil
}

type paid struct {
	TenantID, Plan, UserID string
	PaidUntil              time.Time
}

// MarkInvoicePaid is idempotent: a webhook delivered twice switches the plan once.
func (s *Service) MarkInvoicePaid(ctx context.Context, invoiceID, providerPaymentID, actor string) (bool, error) {
	var res *paid
	err := s.M.Tx(ctx, func(ctx context.Context) error {
		res = nil
		now := time.Now()
		var inv bson.M
		err := s.M.C("invoices").FindOneAndUpdate(ctx, bson.M{"_id": invoiceID, "status": "created"},
			bson.M{"$set": bson.M{"status": "paid", "paid_at": now, "provider_payment_id": providerPaymentID}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&inv)
		if db.NotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		tid := val.Str(inv["tenant_id"])
		var t bson.M
		if err := s.M.C("tenants").FindOne(ctx, bson.M{"_id": tid}).Decode(&t); err != nil {
			if db.NotFound(err) {
				return nil
			}
			return err
		}
		plan, billing := val.Str(inv["plan"]), val.Str(inv["billing"])
		from := now
		if val.Str(t["plan"]) == plan && val.Str(t["billing"]) == billing {
			if pu, ok := val.Time(t["paid_until"]); ok && pu.After(now) {
				from = pu
			}
		}
		paidUntil := from.Add(Term(billing))
		year := now.In(val.IST).Year()
		var counter bson.M
		if err := s.M.C("counters").FindOneAndUpdate(ctx, bson.M{"_id": fmt.Sprintf("gst_invoice_%d", year)}, bson.M{"$inc": bson.M{"seq": 1}},
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&counter); err != nil {
			return err
		}
		gst := fmt.Sprintf("DDI-%d-%06d", year, val.IntOr(counter["seq"], 1))
		if _, err := s.M.C("invoices").UpdateOne(ctx, bson.M{"_id": invoiceID}, bson.M{"$set": bson.M{"gst_invoice_no": gst}}); err != nil {
			return err
		}
		status := "active"
		if val.Str(t["status"]) == "suspended" {
			status = "suspended"
		}
		if _, err := s.M.C("tenants").UpdateOne(ctx, bson.M{"_id": tid}, bson.M{"$set": bson.M{
			"plan": plan, "billing": billing, "paid_until": paidUntil, "last_paid_amount": inv["price"], "status": status, "updated_at": now,
		}}); err != nil {
			return err
		}
		if _, err := s.M.C("subscriptions").UpdateOne(ctx, bson.M{"_id": tid}, bson.M{"$set": bson.M{
			"tenant_id": tid, "plan": plan, "billing": billing, "paid_until": paidUntil, "last_invoice_id": invoiceID, "updated_at": now,
		}}, options.UpdateOne().SetUpsert(true)); err != nil {
			return err
		}
		if _, err := changelog.Log(ctx, s.M.DB, tid, changelog.Entry{Table: "tenants", ID: tid, Op: "upsert"}); err != nil {
			return err
		}
		res = &paid{TenantID: tid, Plan: plan, UserID: val.Str(inv["user_id"]), PaidUntil: paidUntil}
		// The owner's WhatsApp confirmation goes out from the queue, so it is
		// never lost and never sent for a payment that rolled back.
		_, err = s.M.C("jobs").InsertOne(ctx, jobs.Doc(jobs.Job{Kind: jobs.NotifyOwner, TenantID: tid, Ref: invoiceID, Payload: bson.M{
			"user_id": res.UserID, "template": "dd_subscription_paid",
			"params": bson.A{plans.Plans[plan].Name, paidUntil.In(val.IST).Format("2006-01-02")},
		}}))
		return err
	})
	if err != nil || res == nil {
		return false, err
	}
	if err := privacy.Audit(ctx, s.M.DB, actor, "plan_paid", res.TenantID, bson.M{"invoice_id": invoiceID, "plan": res.Plan}); err != nil {
		slog.Error("audit_failed", "err", err)
	}
	// Messages held for the plan or its quota can go now.
	if _, err := jobs.Release(ctx, s.M.DB, bson.M{"tenant_id": res.TenantID, "hold_reason": bson.M{"$in": bson.A{"plan", "quota"}}}); err != nil {
		slog.Error("release_failed", "err", err)
	}
	return true, nil
}
