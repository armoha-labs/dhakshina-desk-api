package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/config"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/fieldcrypt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

// Settings say which gateway takes subscription payments, with the keys for
// each one. The platform admin saves them in platform_settings, secrets
// encrypted; until then they come from the environment. Keys for both
// gateways are kept, so a payment started before a switch still confirms.
type Settings struct {
	Active                string // mock, razorpay, cashfree
	RazorpayKeyID         string
	RazorpayKeySecret     string
	RazorpayWebhookSecret string
	CashfreeAppID         string
	CashfreeSecret        string
	CashfreeProduction    bool
	Source                string // "admin" once saved from the console, else "env"
	UpdatedAt             time.Time
	UpdatedBy             string
}

const (
	settingsCollection = "platform_settings"
	settingsID         = "payment_gateway"
)

// FromConfig is the environment's settings, used until the admin saves some.
func FromConfig(cfg *config.Config) Settings {
	return Settings{
		Active:                cfg.Gateway,
		RazorpayKeyID:         cfg.RazorpayKeyID,
		RazorpayKeySecret:     cfg.RazorpayKeySecret,
		RazorpayWebhookSecret: cfg.RazorpayWebhookKey,
		CashfreeAppID:         cfg.CashfreeAppID,
		CashfreeSecret:        cfg.CashfreeSecretKey,
		CashfreeProduction:    cfg.CashfreeEnv == "production",
		Source:                "env",
	}
}

// For returns the gateway with that name built from these settings, or nil.
func (s Settings) For(cfg *config.Config, name string) Gateway {
	switch name {
	case "razorpay":
		return &Razorpay{KeyID: s.RazorpayKeyID, KeySecret: s.RazorpayKeySecret, WebhookSecret: s.RazorpayWebhookSecret}
	case "cashfree":
		return &Cashfree{AppID: s.CashfreeAppID, Secret: s.CashfreeSecret, Production: s.CashfreeProduction}
	case "mock":
		if cfg.IsProd() {
			return nil
		}
		return &Mock{APIBase: cfg.APIBaseURL}
	}
	return nil
}

// Live reports whether that gateway's keys take real money.
func (s Settings) Live(name string) bool {
	switch name {
	case "razorpay":
		return strings.HasPrefix(s.RazorpayKeyID, "rzp_live_")
	case "cashfree":
		return s.CashfreeProduction
	}
	return false
}

// Load reads the saved settings, falling back to the environment.
func Load(ctx context.Context, db *mongo.Database, box *fieldcrypt.Box, cfg *config.Config) (Settings, error) {
	var d bson.M
	err := db.Collection(settingsCollection).FindOne(ctx, bson.M{"_id": settingsID}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return FromConfig(cfg), nil
	}
	if err != nil {
		return Settings{}, err
	}
	rp, cf := val.M(d["razorpay"]), val.M(d["cashfree"])
	s := Settings{
		Active:                val.Str(d["active"]),
		RazorpayKeyID:         val.Str(rp["key_id"]),
		RazorpayKeySecret:     box.DecryptOr(rp["key_secret_enc"]),
		RazorpayWebhookSecret: box.DecryptOr(rp["webhook_secret_enc"]),
		CashfreeAppID:         val.Str(cf["app_id"]),
		CashfreeSecret:        box.DecryptOr(cf["secret_enc"]),
		CashfreeProduction:    cf["production"] == true,
		Source:                "admin",
		UpdatedBy:             val.Str(d["updated_by"]),
	}
	s.UpdatedAt, _ = val.Time(d["updated_at"])
	return s, nil
}

// Save stores the settings with every secret encrypted.
func Save(ctx context.Context, db *mongo.Database, box *fieldcrypt.Box, s Settings, by string) error {
	enc := func(v string) any {
		if v == "" {
			return nil
		}
		return box.Encrypt(v)
	}
	_, err := db.Collection(settingsCollection).ReplaceOne(ctx, bson.M{"_id": settingsID}, bson.M{
		"_id":    settingsID,
		"active": s.Active,
		"razorpay": bson.M{
			"key_id": s.RazorpayKeyID, "key_secret_enc": enc(s.RazorpayKeySecret), "webhook_secret_enc": enc(s.RazorpayWebhookSecret),
		},
		"cashfree":   bson.M{"app_id": s.CashfreeAppID, "secret_enc": enc(s.CashfreeSecret), "production": s.CashfreeProduction},
		"updated_at": time.Now(),
		"updated_by": by,
	}, options.Replace().SetUpsert(true))
	return err
}

// ErrKeysRejected means the gateway answered and refused the keys.
var ErrKeysRejected = errors.New("keys rejected")

// Check asks the gateway whether it accepts the keys, without charging
// anything. Razorpay lists one order; Cashfree looks up an order that
// doesn't exist, which answers "not found" only to valid keys.
func Check(ctx context.Context, g Gateway) error {
	var req *http.Request
	switch g := g.(type) {
	case *Razorpay:
		if g.KeyID == "" || g.KeySecret == "" {
			return ErrKeysRejected
		}
		req, _ = http.NewRequestWithContext(ctx, "GET", "https://api.razorpay.com/v1/orders?count=1", nil)
		req.SetBasicAuth(g.KeyID, g.KeySecret)
	case *Cashfree:
		if g.AppID == "" || g.Secret == "" {
			return ErrKeysRejected
		}
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		api := "https://sandbox.cashfree.com/pg"
		if g.Production {
			api = "https://api.cashfree.com/pg"
		}
		req, _ = http.NewRequestWithContext(ctx, "GET", api+"/orders/dd-key-check-"+hex.EncodeToString(b), nil)
		req.Header.Set("x-client-id", g.AppID)
		req.Header.Set("x-client-secret", g.Secret)
		req.Header.Set("x-api-version", "2023-08-01")
	default:
		return nil
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	switch {
	case res.StatusCode == 401 || res.StatusCode == 403:
		return ErrKeysRejected
	case res.StatusCode < 300, res.StatusCode == 404:
		return nil
	}
	return fmt.Errorf("the gateway answered %d", res.StatusCode)
}
