// Package config reads settings from the environment. Secrets are never
// defaulted in production; development gets fixed throwaway values so the
// service starts with only MONGODB_URI set.
package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Env  string // "production" or anything else
	Addr string // listen address, default :8080
	Role string // api, worker or all

	MongoURI      string
	MongoAdminURI string // optional: a database user that can read only platform collections
	MongoDB       string

	WebBaseURL string // where parent and checkout pages live (links point here)
	APIBaseURL string // this service's public origin (gateway notify URLs, mock pay)

	JWTSecret        string
	LinkSecret       string
	FieldEncKey      string
	AdminTokenSecret string
	AdminUsers       string // email=scrypt$salt$hash,...

	Gateway             string // mock, razorpay, cashfree
	RazorpayKeyID       string
	RazorpayKeySecret   string
	RazorpayWebhookKey  string
	CashfreeAppID       string
	CashfreeSecretKey   string
	CashfreeEnv         string
	WhatsAppToken       string
	WhatsAppPhoneID     string
	WhatsAppAppSecret   string
	WhatsAppVerifyToken string
	WhatsAppAPIBase     string
}

func (c *Config) IsProd() bool { return c.Env == "production" }

func Load() (*Config, error) {
	c := &Config{
		Env:                 os.Getenv("APP_ENV"),
		Addr:                or(os.Getenv("ADDR"), ":"+or(os.Getenv("PORT"), "8080")),
		Role:                or(os.Getenv("ROLE"), "all"),
		MongoURI:            os.Getenv("MONGODB_URI"),
		MongoAdminURI:       os.Getenv("MONGODB_ADMIN_URI"),
		MongoDB:             or(os.Getenv("MONGODB_DB"), "dhakshina_desk"),
		WebBaseURL:          strings.TrimRight(or(os.Getenv("WEB_BASE_URL"), "http://localhost:3000"), "/"),
		APIBaseURL:          strings.TrimRight(or(os.Getenv("API_BASE_URL"), "http://localhost:8080"), "/"),
		AdminUsers:          os.Getenv("ADMIN_USERS"),
		Gateway:             or(os.Getenv("PAYMENT_GATEWAY"), "mock"),
		RazorpayKeyID:       os.Getenv("RAZORPAY_KEY_ID"),
		RazorpayKeySecret:   os.Getenv("RAZORPAY_KEY_SECRET"),
		RazorpayWebhookKey:  os.Getenv("RAZORPAY_WEBHOOK_SECRET"),
		CashfreeAppID:       os.Getenv("CASHFREE_APP_ID"),
		CashfreeSecretKey:   os.Getenv("CASHFREE_SECRET_KEY"),
		CashfreeEnv:         or(os.Getenv("CASHFREE_ENV"), "sandbox"),
		WhatsAppToken:       os.Getenv("WHATSAPP_TOKEN"),
		WhatsAppPhoneID:     os.Getenv("WHATSAPP_PHONE_NUMBER_ID"),
		WhatsAppAppSecret:   os.Getenv("WHATSAPP_APP_SECRET"),
		WhatsAppVerifyToken: os.Getenv("WHATSAPP_VERIFY_TOKEN"),
		WhatsAppAPIBase:     or(os.Getenv("WHATSAPP_API_BASE"), "https://graph.facebook.com/v21.0"),
	}
	var err error
	for _, s := range []struct {
		dst  *string
		name string
	}{
		{&c.JWTSecret, "JWT_SECRET"},
		{&c.LinkSecret, "LINK_SECRET"},
		{&c.FieldEncKey, "FIELD_ENC_KEY"},
		{&c.AdminTokenSecret, "ADMIN_TOKEN_SECRET"},
	} {
		if *s.dst, err = c.secret(s.name); err != nil {
			return nil, err
		}
	}
	if c.MongoURI == "" {
		return nil, fmt.Errorf("MONGODB_URI is not set")
	}
	switch c.Role {
	case "api", "worker", "all":
	default:
		return nil, fmt.Errorf("ROLE must be api, worker or all")
	}
	if c.IsProd() && c.Gateway == "mock" {
		return nil, fmt.Errorf("PAYMENT_GATEWAY=mock cannot be used in production")
	}
	return c, nil
}

func (c *Config) secret(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	if c.IsProd() {
		return "", fmt.Errorf("%s is not set", name)
	}
	return "dev-only-" + name + "-do-not-use-in-production", nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
