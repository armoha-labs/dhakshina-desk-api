// Package app wires the shared dependencies every module uses and the
// bearer-token check for teacher calls.
package app

import (
	"net/http"
	"regexp"

	"github.com/armohalabs/dhakshina-desk-api/internal/config"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/fieldcrypt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/jwt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/links"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/whatsapp"
)

type App struct {
	Cfg   *config.Config
	M     *db.Mongo
	Box   *fieldcrypt.Box
	Links *links.Signer
	WA    *whatsapp.Client
}

func New(cfg *config.Config, m *db.Mongo) *App {
	return &App{
		Cfg:   cfg,
		M:     m,
		Box:   fieldcrypt.New(cfg.FieldEncKey),
		Links: links.New(cfg.LinkSecret),
		WA:    whatsapp.New(cfg.WhatsAppToken, cfg.WhatsAppPhoneID, cfg.WhatsAppAppSecret, cfg.WhatsAppAPIBase),
	}
}

// Principal is the signed-in teacher from the access token.
type Principal struct {
	UserID   string
	TenantID string // empty before the class exists
	Role     string // owner, co_teacher
}

var bearer = regexp.MustCompile(`(?i)^Bearer\s+(.+)$`)

// Auth verifies the access token on a teacher call.
func (a *App) Auth(r *http.Request) (*Principal, error) {
	m := bearer.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return nil, httpx.Err(httpx.Unauthorized, "Please sign in.")
	}
	c, err := jwt.Verify(m[1], a.Cfg.JWTSecret)
	if err == jwt.ErrExpired {
		return nil, httpx.Err(httpx.TokenExpired, "Session expired.")
	}
	if err != nil || c.Str("sub") == "" || c["adm"] != nil {
		return nil, httpx.Err(httpx.Unauthorized, "Please sign in.")
	}
	p := &Principal{UserID: c.Str("sub"), TenantID: c.Str("tid"), Role: c.Str("role")}
	if p.TenantID != "" && p.Role == "" {
		p.Role = "co_teacher"
	}
	return p, nil
}

// Teacher requires a token scoped to a class.
func (a *App) Teacher(r *http.Request) (*Principal, error) {
	p, err := a.Auth(r)
	if err != nil {
		return nil, err
	}
	if p.TenantID == "" {
		return nil, httpx.Err(httpx.Forbidden, "Set up your class first.")
	}
	return p, nil
}

// WebURL builds a link on the website.
func (a *App) WebURL(path string) string { return a.Cfg.WebBaseURL + path }
