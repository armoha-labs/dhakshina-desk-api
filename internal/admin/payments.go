package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/billing/gateway"
	"github.com/armohalabs/dhakshina-desk-api/internal/docs"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

// Payment settings: which gateway collects subscription fees, and its keys.
// Secrets are write-only: the console sees whether one is saved and its last
// four characters, never the secret itself.

var razorpayKeyID = regexp.MustCompile(`^rzp_(test|live)_[A-Za-z0-9]{6,40}$`)

func hint(secret string) string {
	switch {
	case secret == "":
		return ""
	case len(secret) < 12:
		return "Saved"
	}
	return "••••" + secret[len(secret)-4:]
}

func (s *Service) paymentSettings(w http.ResponseWriter, r *http.Request) error {
	set, err := gateway.Load(r.Context(), s.M.AdminDB, s.Box, s.Cfg)
	if err != nil {
		return err
	}
	var updated any
	if !set.UpdatedAt.IsZero() {
		updated = docs.Plain(set.UpdatedAt)
	}
	hook := s.Cfg.APIBaseURL + "/v1/billing/webhook/"
	httpx.JSON(w, 200, bson.M{
		"active": set.Active, "source": set.Source, "updated_at": updated, "updated_by": set.UpdatedBy,
		"production":   s.Cfg.IsProd(),
		"mock_allowed": !s.Cfg.IsProd(),
		"razorpay": bson.M{
			"key_id": set.RazorpayKeyID, "key_secret": hint(set.RazorpayKeySecret), "webhook_secret": hint(set.RazorpayWebhookSecret),
			"live": set.Live("razorpay"), "webhook_url": hook + "razorpay",
		},
		"cashfree": bson.M{
			"app_id": set.CashfreeAppID, "secret": hint(set.CashfreeSecret), "production": set.CashfreeProduction,
			"live": set.Live("cashfree"), "webhook_url": hook + "cashfree",
		},
	})
	return nil
}

// savePaymentSettings changes only what was sent: a blank secret keeps the
// saved one, so the console never has to hold it.
func (s *Service) savePaymentSettings(w http.ResponseWriter, r *http.Request) error {
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	set, err := gateway.Load(r.Context(), s.M.AdminDB, s.Box, s.Cfg)
	if err != nil {
		return err
	}
	var changed []string
	put := func(field string, dst *string, v string, secret bool) {
		if (secret && v == "") || v == *dst {
			return
		}
		*dst = v
		changed = append(changed, field)
	}
	rp, cf := val.M(b["razorpay"]), val.M(b["cashfree"])
	if rp != nil {
		put("razorpay_key_id", &set.RazorpayKeyID, httpx.Str(rp["key_id"], 60), false)
		put("razorpay_key_secret", &set.RazorpayKeySecret, httpx.Str(rp["key_secret"], 200), true)
		put("razorpay_webhook_secret", &set.RazorpayWebhookSecret, httpx.Str(rp["webhook_secret"], 200), true)
	}
	if cf != nil {
		put("cashfree_app_id", &set.CashfreeAppID, httpx.Str(cf["app_id"], 100), false)
		put("cashfree_secret", &set.CashfreeSecret, httpx.Str(cf["secret"], 200), true)
		if p, ok := cf["production"].(bool); ok && p != set.CashfreeProduction {
			set.CashfreeProduction = p
			changed = append(changed, "cashfree_environment")
		}
	}
	if a := httpx.Str(b["active"], 20); a != "" && a != set.Active {
		set.Active = a
		changed = append(changed, "active")
	}

	if set.RazorpayKeyID != "" && !razorpayKeyID.MatchString(set.RazorpayKeyID) {
		return httpx.Err(httpx.BadRequest, "A Razorpay key id starts with rzp_test_ or rzp_live_.")
	}
	switch set.Active {
	case "razorpay":
		if set.RazorpayKeyID == "" || set.RazorpayKeySecret == "" || set.RazorpayWebhookSecret == "" {
			return httpx.Err(httpx.BadRequest, "Add the Razorpay key id, key secret and webhook secret before making Razorpay active.")
		}
	case "cashfree":
		if set.CashfreeAppID == "" || set.CashfreeSecret == "" {
			return httpx.Err(httpx.BadRequest, "Add the Cashfree app id and secret key before making Cashfree active.")
		}
	case "mock":
		if s.Cfg.IsProd() {
			return httpx.Err(httpx.BadRequest, "The test gateway can't take payments in production.")
		}
	default:
		return httpx.Err(httpx.BadRequest, "Choose Razorpay or Cashfree.")
	}

	if len(changed) == 0 && set.Source == "admin" {
		httpx.NoContent(w)
		return nil
	}
	if err := gateway.Save(r.Context(), s.M.AdminDB, s.Box, set, staff(r)); err != nil {
		return err
	}
	// Names of what changed only: secrets never reach the log.
	s.log(r, "payment_settings_changed", "", bson.M{"active": set.Active, "changed": changed})
	httpx.NoContent(w)
	return nil
}

// testPaymentKeys asks the gateway whether it accepts the saved keys.
func (s *Service) testPaymentKeys(w http.ResponseWriter, r *http.Request) error {
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	provider := httpx.Str(b["provider"], 20)
	if provider != "razorpay" && provider != "cashfree" {
		return httpx.Err(httpx.BadRequest, "Choose Razorpay or Cashfree.")
	}
	set, err := gateway.Load(r.Context(), s.M.AdminDB, s.Box, s.Cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err = gateway.Check(ctx, set.For(s.Cfg, provider))
	ok := err == nil
	s.log(r, "payment_keys_tested", "", bson.M{"provider": provider, "ok": ok})
	switch {
	case errors.Is(err, gateway.ErrKeysRejected):
		return httpx.Err(httpx.BadRequest, "The gateway refused these keys. Check them in its dashboard and save again.")
	case err != nil:
		slog.Warn("payment_key_check_failed", "provider", provider, "err", err)
		return httpx.Err(httpx.ServerError, "Couldn't reach the gateway just now. Try again in a minute.")
	}
	httpx.JSON(w, 200, bson.M{"ok": true, "live": set.Live(provider)})
	return nil
}
