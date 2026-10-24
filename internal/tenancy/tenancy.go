// Package tenancy creates classes (tenants) and validates the class fields a
// teacher can edit.
package tenancy

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/docs"
	"github.com/armohalabs/dhakshina-desk-api/internal/identity"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/upi"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

var prefixRE = regexp.MustCompile(`^[A-Za-z0-9]{1,8}$`)

// CleanFields validates the editable class fields. A key absent from the
// body is left alone; null clears the optional ones.
func CleanFields(b map[string]any) (bson.M, error) {
	out := bson.M{}
	if s := httpx.Str(b["name"], 80); s != "" {
		out["name"] = s
	}
	for _, k := range []string{"owner_name", "upi_name", "city", "grievance_contact"} {
		v, present := b[k]
		if present && v == nil {
			out[k] = nil
		}
		if s := httpx.Str(v, 80); s != "" {
			out[k] = s
		}
	}
	if s := httpx.Str(b["upi_id"], 120); s != "" {
		if !upi.IsVPA(s) {
			return nil, httpx.Err(httpx.BadRequest, "That UPI ID does not look right. It should look like name@bank.")
		}
		out["upi_id"] = s
	}
	if s := httpx.Str(b["receipt_prefix"], 8); s != "" {
		if !prefixRE.MatchString(s) {
			return nil, httpx.Err(httpx.BadRequest, "Receipt prefix: letters and numbers only.")
		}
		out["receipt_prefix"] = strings.ToUpper(s)
	}
	return out, nil
}

type Service struct {
	*app.App
	Identity *identity.Service
}

func (s *Service) Routes(r chi.Router) {
	r.Method("POST", "/tenants", httpx.Handler(s.create))
}

// Create makes the class with the caller as owner. The id is client-made, so
// a retried setup from the same phone returns the class it already made.
func (s *Service) Create(ctx context.Context, userID string, b map[string]any) (bson.M, error) {
	id, _ := b["id"].(string)
	if !ids.IsUUID(id) {
		id = ids.UUIDv7()
	}
	fields, err := CleanFields(b)
	if err != nil {
		return nil, err
	}
	if fields["name"] == nil {
		return nil, httpx.Err(httpx.BadRequest, "Give your class a name.")
	}
	var existing bson.M
	err = s.M.C("tenants").FindOne(ctx, bson.M{"_id": id}).Decode(&existing)
	if err == nil {
		if n, _ := s.M.C("memberships").CountDocuments(ctx, bson.M{"tenant_id": id, "user_id": userID}); n == 0 {
			return nil, httpx.Err(httpx.Conflict, "That class already exists.")
		}
		return existing, nil
	}
	if !db.NotFound(err) {
		return nil, err
	}
	now := time.Now()
	doc := bson.M{
		"_id": id, "receipt_prefix": "DD",
		"plan": "trial", "billing": nil, "status": "active", "founder": false, "feature_flags": bson.M{},
		"trial_ends_on": now.In(val.IST).AddDate(0, 0, plans.TrialDays).Format("2006-01-02"),
		"paid_until":    nil,
		// Change 1 is the class itself, so a second phone gets it on first sync.
		"change_seq": 1, "receipt_seq": 0,
		"stats":      bson.M{"students": 0, "batches": 0, "at": now},
		"created_by": userID, "created_at": now, "updated_at": now,
	}
	for k, v := range fields {
		doc[k] = v
	}
	err = s.M.Tx(ctx, func(ctx context.Context) error {
		if _, err := s.M.C("tenants").InsertOne(ctx, doc); err != nil {
			return err
		}
		if _, err := s.M.C("change_log").InsertOne(ctx, bson.M{"tenant_id": id, "seq": 1, "table": "tenants", "doc_id": id, "op": "upsert", "at": now}); err != nil {
			return err
		}
		_, err := s.M.C("memberships").InsertOne(ctx, bson.M{"_id": ids.UUIDv7(), "tenant_id": id, "user_id": userID, "role": "owner", "created_at": now})
		return err
	})
	if db.IsDup(err) {
		return nil, httpx.Err(httpx.Conflict, "That class already exists.")
	}
	if err != nil {
		return nil, err
	}
	if name := httpx.Str(b["owner_name"], 80); name != "" {
		_, _ = s.M.C("users").UpdateOne(ctx, bson.M{"_id": userID, "name": ""}, bson.M{"$set": bson.M{"name": name}})
	}
	return doc, nil
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Auth(r)
	if err != nil {
		return err
	}
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	t, err := s.Create(r.Context(), p.UserID, b)
	if err != nil {
		return err
	}
	sess, err := s.Identity.IssueSession(r.Context(), p.UserID, val.Str(t["_id"]), "")
	if err != nil {
		return err
	}
	sess["tenant"] = docs.TenantView(t)
	httpx.JSON(w, 201, sess)
	return nil
}
