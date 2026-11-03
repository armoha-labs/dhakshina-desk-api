package billing

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/billing/gateway"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

func (s *Service) Routes(r chi.Router) {
	r.Method("GET", "/plans", httpx.Handler(s.plans))
	r.Method("POST", "/links/upgrade", httpx.Handler(s.upgradeLink))
	r.Method("GET", "/billing/quote", httpx.Handler(s.quote))
	r.Method("POST", "/billing/checkout", httpx.Handler(s.checkout))
	r.Method("GET", "/billing/invoices/{id}", httpx.Handler(s.invoice))
	r.Method("POST", "/billing/webhook/{provider}", httpx.Handler(s.webhook))
	r.Method("GET", "/billing/mock-pay", httpx.Handler(s.mockPay))
}

func plansBody() bson.M { return bson.M{"trial_days": plans.TrialDays, "plans": plans.Plans} }

func (s *Service) plans(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	return json.NewEncoder(w).Encode(plansBody())
}

// payer resolves who is paying: a signed upgrade link from WhatsApp, or a
// signed-in owner on the website.
func (s *Service) payer(r *http.Request, token string) (tenantID, userID string, err error) {
	if token != "" {
		if t, u, ok := s.Links.VerifyUpgrade(token); ok {
			return t, u, nil
		}
		return "", "", httpx.Err(httpx.Unauthorized, "This payment link has expired. Ask for a new one from the app.")
	}
	p, err := s.Teacher(r)
	if err != nil {
		return "", "", httpx.Err(httpx.Unauthorized, "This payment link has expired. Ask for a new one from the app.")
	}
	if p.Role != "owner" {
		return "", "", httpx.Err(httpx.Forbidden, "Only the class owner can change the plan.")
	}
	return p.TenantID, p.UserID, nil
}

func planArgs(plan, billing string) (string, string) {
	if plan == "" {
		plan = "pro"
	}
	if billing != "yearly" {
		billing = "monthly"
	}
	return plan, billing
}

func (s *Service) quote(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	tid, _, err := s.payer(r, q.Get("token"))
	if err != nil {
		return err
	}
	var t bson.M
	if err := s.M.C("tenants").FindOne(r.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		if db.NotFound(err) {
			return httpx.Err(httpx.NotFound, "Class not found.")
		}
		return err
	}
	plan, billing := planArgs(q.Get("plan"), q.Get("billing"))
	qt, err := QuoteFor(t, plan, billing, time.Now())
	if err != nil {
		return err
	}
	stats := val.M(t["stats"])
	httpx.JSON(w, 200, bson.M{
		"tenant": bson.M{"id": tid, "name": t["name"], "plan": plans.StateOf(t, time.Now()).Plan, "billing": t["billing"], "students": val.IntOr(stats["students"], 0)},
		"quote":  qt,
		"plans":  plans.Plans,
	})
	return nil
}

func (s *Service) checkout(w http.ResponseWriter, r *http.Request) error {
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	tid, uid, err := s.payer(r, httpx.Str(b["token"], 2000))
	if err != nil {
		return err
	}
	plan, billing := planArgs(httpx.Str(b["plan"], 20), httpx.Str(b["billing"], 20))
	out, err := s.CreateCheckout(r.Context(), tid, uid, plan, billing)
	if err != nil {
		return err
	}
	httpx.JSON(w, 201, out)
	return nil
}

// invoice is the payment result page's poll; the id is an unguessable UUIDv7.
func (s *Service) invoice(w http.ResponseWriter, r *http.Request) error {
	var inv bson.M
	if err := s.M.C("invoices").FindOne(r.Context(), bson.M{"_id": chi.URLParam(r, "id")}).Decode(&inv); err != nil {
		if db.NotFound(err) {
			return httpx.Err(httpx.NotFound, "Invoice not found.")
		}
		return err
	}
	var t bson.M
	_ = s.M.C("tenants").FindOne(r.Context(), bson.M{"_id": inv["tenant_id"]}).Decode(&t)
	httpx.JSON(w, 200, bson.M{"status": inv["status"], "plan": inv["plan"], "tenant_name": val.Str(t["name"]), "gst_invoice_no": inv["gst_invoice_no"]})
	return nil
}

// webhook verifies the signature over the raw body, then switches the plan.
// Events we ignore still get 200, so the gateway stops retrying.
func (s *Service) webhook(w http.ResponseWriter, r *http.Request) error {
	provider := chi.URLParam(r, "provider")
	set, err := gateway.Load(r.Context(), s.M.DB, s.Box, s.Cfg)
	if err != nil {
		return err
	}
	gw := set.For(s.Cfg, provider)
	if gw == nil || gw.Name() == "mock" {
		return httpx.Err(httpx.NotFound, "Unknown gateway.")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return httpx.Err(httpx.BadRequest, "Could not read the event.")
	}
	evt, err := gw.ParseWebhook(raw, r.Header)
	if err != nil {
		return err
	}
	if evt == nil {
		httpx.JSON(w, 200, bson.M{"ok": true, "ignored": true})
		return nil
	}
	n, err := s.M.C("invoices").CountDocuments(r.Context(), bson.M{"_id": evt.InvoiceID, "provider": provider})
	if err != nil {
		return err
	}
	if n == 0 {
		httpx.JSON(w, 200, bson.M{"ok": true, "ignored": true})
		return nil
	}
	if _, err := s.MarkInvoicePaid(r.Context(), evt.InvoiceID, evt.ProviderPaymentID, "gateway:"+provider); err != nil {
		return err
	}
	httpx.JSON(w, 200, bson.M{"ok": true})
	return nil
}

// mockPay completes a mock-gateway payment. Development only.
func (s *Service) mockPay(w http.ResponseWriter, r *http.Request) error {
	if s.Cfg.IsProd() {
		return httpx.Err(httpx.NotFound, "Not found.")
	}
	id := r.URL.Query().Get("invoice")
	n, err := s.M.C("invoices").CountDocuments(r.Context(), bson.M{"_id": id, "provider": "mock"})
	if err != nil {
		return err
	}
	if n == 0 {
		return httpx.Err(httpx.NotFound, "Unknown invoice.")
	}
	suffix := id
	if len(suffix) > 6 {
		suffix = suffix[len(suffix)-6:]
	}
	if _, err := s.MarkInvoicePaid(r.Context(), id, "mock_pay_"+suffix, "gateway:mock"); err != nil {
		return err
	}
	httpx.Redirect303(w, s.WebURL("/subscribe/done?invoice="+url.QueryEscape(id)))
	return nil
}

// upgradeLink returns a signed checkout link for the class owner. The app's
// plan button opens it in the browser, where payment happens, so the owner
// lands on checkout already signed in. The link lasts 14 days.
func (s *Service) upgradeLink(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Teacher(r)
	if err != nil {
		return err
	}
	if p.Role != "owner" {
		return httpx.Err(httpx.Forbidden, "Only the class owner can change the plan.")
	}
	tok, _ := s.Links.Upgrade(p.TenantID, p.UserID)
	httpx.JSON(w, 200, bson.M{"url": s.WebURL("/subscribe/" + tok)})
	return nil
}
