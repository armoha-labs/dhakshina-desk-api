package syncer

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/docs"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/store"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

func (s *Service) Routes(r chi.Router) {
	r.Method("POST", "/sync", httpx.Handler(s.sync))
	r.Method("GET", "/class", httpx.Handler(s.class))
}

func (s *Service) sync(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Teacher(r)
	if err != nil {
		return err
	}
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	out, err := s.Sync(r.Context(), p.TenantID, b)
	var rj *Reject
	if errors.As(err, &rj) {
		code := httpx.BadRequest
		if rj.Code == "not_found" {
			code = httpx.NotFound
		}
		return httpx.Err(code, rj.Message)
	}
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, out)
	return nil
}

// class is the website's teacher setup page: the class, its batches and fee
// plans and the change number to sync edits from.
func (s *Service) class(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Teacher(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	sc := store.For(s.M.DB, p.TenantID)
	t, err := sc.Tenant(ctx)
	if err != nil {
		return err
	}
	if t == nil {
		return httpx.Err(httpx.NotFound, "Class not found.")
	}
	live := bson.M{"deleted": bson.M{"$ne": true}}
	byName := options.Find().SetSort(bson.D{{Key: "name", Value: 1}})
	batches, err := sc.Find(ctx, "batches", live, byName)
	if err != nil {
		return err
	}
	feePlans, err := sc.Find(ctx, "fee_plans", live, byName)
	if err != nil {
		return err
	}
	ent, err := plans.Entitlements(ctx, sc, t)
	if err != nil {
		return err
	}
	tv := docs.TenantView(t)
	tv["plan"] = plans.StateOf(t, time.Now()).Plan
	tv["billing"] = t["billing"]
	tv["trial_ends_on"] = t["trial_ends_on"]
	tv["paid_until"] = nil
	if pu, ok := val.Time(t["paid_until"]); ok {
		tv["paid_until"] = val.Day(pu)
	}
	stats := val.M(t["stats"])
	tv["stats"] = bson.M{"students": val.IntOr(stats["students"], 0), "batches": val.IntOr(stats["batches"], 0)}
	shape := func(table string, list []bson.M) []bson.M {
		out := make([]bson.M, len(list))
		for i, d := range list {
			out[i] = docs.ToAPI(s.Box, table, d)
		}
		return out
	}
	httpx.JSON(w, 200, bson.M{
		"seq":          val.IntOr(t["change_seq"], 0),
		"role":         p.Role,
		"tenant":       tv,
		"batches":      shape("batches", batches),
		"fee_plans":    shape("fee_plans", feePlans),
		"entitlements": ent,
	})
	return nil
}
