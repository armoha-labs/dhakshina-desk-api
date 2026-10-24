// Package server wires the modules into one router. Every route is served at
// /v1/... (the app's base URL is the API origin) and also at /api/v1/... so
// links built against the old Next.js API keep working.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/admin"
	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/billing"
	"github.com/armohalabs/dhakshina-desk-api/internal/collections"
	"github.com/armohalabs/dhakshina-desk-api/internal/identity"
	"github.com/armohalabs/dhakshina-desk-api/internal/messaging"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/syncer"
	"github.com/armohalabs/dhakshina-desk-api/internal/tenancy"
)

type Modules struct {
	Identity *identity.Service
	Tenancy  *tenancy.Service
	Sync     *syncer.Service
	Family   *collections.Service
	Billing  *billing.Service
	Worker   *messaging.Worker
	Admin    *admin.Service
}

func NewModules(a *app.App) *Modules {
	id := &identity.Service{App: a}
	fam := &collections.Service{App: a}
	return &Modules{
		Identity: id,
		Tenancy:  &tenancy.Service{App: a, Identity: id},
		Sync:     &syncer.Service{App: a},
		Family:   fam,
		Billing:  &billing.Service{App: a},
		Worker:   messaging.NewWorker(a, fam),
		Admin:    admin.New(a),
	}
}

func Router(a *app.App, m *Modules) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP, httpx.Log, httpx.Recover, middleware.Timeout(30*time.Second), middleware.Compress(5, "application/json", "text/csv"))
	v1 := func(r chi.Router) {
		r.Get("/health", func(w http.ResponseWriter, req *http.Request) {
			ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
			defer cancel()
			if err := a.M.DB.RunCommand(ctx, bson.M{"ping": 1}).Err(); err != nil {
				httpx.JSON(w, 503, bson.M{"ok": false, "db": "down"})
				return
			}
			httpx.JSON(w, 200, bson.M{"ok": true, "db": "up"})
		})
		m.Identity.Routes(r)
		m.Tenancy.Routes(r)
		m.Sync.Routes(r)
		m.Family.Routes(r)
		m.Billing.Routes(r)
		m.Worker.Routes(r)
		m.Admin.Routes(r)
	}
	r.Route("/v1", v1)
	r.Route("/api/v1", v1)
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		httpx.Fail(w, req, httpx.Err(httpx.NotFound, "Not found."))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		httpx.Fail(w, req, httpx.Err(httpx.NotFound, "Not found."))
	})
	return r
}
