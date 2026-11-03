// Package gateway holds the subscription payment gateways. Razorpay and
// Cashfree sit behind one interface; the platform admin picks one and saves
// its keys (see Settings), else PAYMENT_GATEWAY picks one. The mock gateway
// is for local runs only.
package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/armohalabs/dhakshina-desk-api/internal/config"
)

type OrderInput struct {
	InvoiceID   string
	AmountPaise int64
	CustomerID  string
	Phone       string
	Name        string
	Description string
	ReturnURL   string
	NotifyURL   string
}

// WebhookResult is a verified payment event for one of our invoices.
type WebhookResult struct {
	InvoiceID         string
	ProviderPaymentID string
}

type Gateway interface {
	Name() string
	// CreateOrder returns the provider's order id and what the checkout page
	// needs to open the gateway's own payment screen.
	CreateOrder(ctx context.Context, in OrderInput) (string, map[string]any, error)
	// ParseWebhook verifies the signature over the raw body; nil when the
	// event is not ours or not a successful payment.
	ParseWebhook(raw []byte, h http.Header) (*WebhookResult, error)
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// ByName returns the environment's gateway with that name, or nil.
func ByName(cfg *config.Config, name string) Gateway { return FromConfig(cfg).For(cfg, name) }

// Razorpay ---------------------------------------------------------------

type Razorpay struct{ KeyID, KeySecret, WebhookSecret string }

func (*Razorpay) Name() string { return "razorpay" }

func (g *Razorpay) CreateOrder(ctx context.Context, in OrderInput) (string, map[string]any, error) {
	if g.KeyID == "" || g.KeySecret == "" {
		return "", nil, errors.New("razorpay keys are not set")
	}
	receipt := in.InvoiceID
	if len(receipt) > 40 {
		receipt = receipt[:40]
	}
	body, _ := json.Marshal(map[string]any{
		"amount": in.AmountPaise, "currency": "INR", "receipt": receipt,
		"notes": map[string]string{"invoice_id": in.InvoiceID, "tenant_id": in.CustomerID},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.razorpay.com/v1/orders", bytes.NewReader(body))
	req.SetBasicAuth(g.KeyID, g.KeySecret)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	var out struct {
		ID    string `json:"id"`
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode >= 300 || out.ID == "" {
		msg := fmt.Sprint(res.StatusCode)
		if out.Error != nil {
			msg = out.Error.Description
		}
		return "", nil, fmt.Errorf("razorpay order failed: %s", msg)
	}
	return out.ID, map[string]any{
		"provider": "razorpay", "key_id": g.KeyID, "order_id": out.ID, "amount": in.AmountPaise,
		"name": "Dhakshina Desk", "description": in.Description,
		"prefill": map[string]string{"contact": in.Phone, "name": in.Name},
	}, nil
}

func (g *Razorpay) ParseWebhook(raw []byte, h http.Header) (*WebhookResult, error) {
	sig := h.Get("X-Razorpay-Signature")
	if g.WebhookSecret == "" || sig == "" {
		return nil, nil
	}
	m := hmac.New(sha256.New, []byte(g.WebhookSecret))
	m.Write(raw)
	if !hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(sig)) {
		return nil, nil
	}
	var evt struct {
		Event   string `json:"event"`
		Payload struct {
			Order *struct {
				Entity struct {
					Notes map[string]any `json:"notes"`
				} `json:"entity"`
			} `json:"order"`
			Payment *struct {
				Entity struct {
					ID    string         `json:"id"`
					Notes map[string]any `json:"notes"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, nil
	}
	if evt.Event != "order.paid" && evt.Event != "payment.captured" {
		return nil, nil
	}
	var inv, pay string
	if evt.Payload.Order != nil {
		inv, _ = evt.Payload.Order.Entity.Notes["invoice_id"].(string)
	}
	if evt.Payload.Payment != nil {
		pay = evt.Payload.Payment.Entity.ID
		if inv == "" {
			inv, _ = evt.Payload.Payment.Entity.Notes["invoice_id"].(string)
		}
	}
	if inv == "" {
		return nil, nil
	}
	return &WebhookResult{InvoiceID: inv, ProviderPaymentID: pay}, nil
}

// Cashfree ---------------------------------------------------------------

type Cashfree struct {
	AppID, Secret string
	Production    bool
}

func (*Cashfree) Name() string { return "cashfree" }

func (g *Cashfree) mode() string {
	if g.Production {
		return "production"
	}
	return "sandbox"
}

func (g *Cashfree) CreateOrder(ctx context.Context, in OrderInput) (string, map[string]any, error) {
	if g.AppID == "" || g.Secret == "" {
		return "", nil, errors.New("cashfree keys are not set")
	}
	api := "https://sandbox.cashfree.com/pg"
	if g.Production {
		api = "https://api.cashfree.com/pg"
	}
	body, _ := json.Marshal(map[string]any{
		"order_id": in.InvoiceID, "order_amount": float64(in.AmountPaise) / 100, "order_currency": "INR", "order_note": in.Description,
		"customer_details": map[string]string{"customer_id": in.CustomerID, "customer_phone": strings.TrimPrefix(in.Phone, "+91"), "customer_name": in.Name},
		"order_meta":       map[string]string{"return_url": in.ReturnURL, "notify_url": in.NotifyURL},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", api+"/orders", bytes.NewReader(body))
	req.Header.Set("x-client-id", g.AppID)
	req.Header.Set("x-client-secret", g.Secret)
	req.Header.Set("x-api-version", "2023-08-01")
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	var out struct {
		CfOrderID        any    `json:"cf_order_id"`
		PaymentSessionID string `json:"payment_session_id"`
		Message          string `json:"message"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode >= 300 || out.PaymentSessionID == "" {
		msg := out.Message
		if msg == "" {
			msg = fmt.Sprint(res.StatusCode)
		}
		return "", nil, fmt.Errorf("cashfree order failed: %s", msg)
	}
	oid := in.InvoiceID
	if out.CfOrderID != nil {
		oid = fmt.Sprint(out.CfOrderID)
	}
	return oid, map[string]any{"provider": "cashfree", "mode": g.mode(), "payment_session_id": out.PaymentSessionID}, nil
}

func (g *Cashfree) ParseWebhook(raw []byte, h http.Header) (*WebhookResult, error) {
	sig, ts := h.Get("X-Webhook-Signature"), h.Get("X-Webhook-Timestamp")
	if g.Secret == "" || sig == "" || ts == "" {
		return nil, nil
	}
	m := hmac.New(sha256.New, []byte(g.Secret))
	m.Write([]byte(ts))
	m.Write(raw)
	if !hmac.Equal([]byte(base64.StdEncoding.EncodeToString(m.Sum(nil))), []byte(sig)) {
		return nil, nil
	}
	var evt struct {
		Type string `json:"type"`
		Data struct {
			Order struct {
				OrderID string `json:"order_id"`
			} `json:"order"`
			Payment *struct {
				CfPaymentID   any    `json:"cf_payment_id"`
				PaymentStatus string `json:"payment_status"`
			} `json:"payment"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, nil
	}
	if evt.Type != "PAYMENT_SUCCESS_WEBHOOK" || evt.Data.Payment == nil || evt.Data.Payment.PaymentStatus != "SUCCESS" {
		return nil, nil
	}
	var pay string
	if evt.Data.Payment.CfPaymentID != nil {
		pay = fmt.Sprint(evt.Data.Payment.CfPaymentID)
		if f, ok := evt.Data.Payment.CfPaymentID.(float64); ok {
			pay = fmt.Sprintf("%.0f", f)
		}
	}
	return &WebhookResult{InvoiceID: evt.Data.Order.OrderID, ProviderPaymentID: pay}, nil
}

// Mock -------------------------------------------------------------------

// Mock "pays" through GET /v1/billing/mock-pay so the flow can be tried
// without gateway keys. Refused in production.
type Mock struct{ APIBase string }

func (*Mock) Name() string { return "mock" }

func (g *Mock) CreateOrder(_ context.Context, in OrderInput) (string, map[string]any, error) {
	return "mock_" + in.InvoiceID, map[string]any{"provider": "mock", "url": g.APIBase + "/v1/billing/mock-pay?invoice=" + url.QueryEscape(in.InvoiceID)}, nil
}

func (*Mock) ParseWebhook([]byte, http.Header) (*WebhookResult, error) { return nil, nil }
