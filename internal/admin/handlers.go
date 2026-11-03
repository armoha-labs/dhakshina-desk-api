package admin

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/docs"
	"github.com/armohalabs/dhakshina-desk-api/internal/jobs"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/jwt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
	"github.com/armohalabs/dhakshina-desk-api/internal/privacy"
)

type ctxKey struct{}

func (s *Service) Routes(r chi.Router) {
	r.Method("POST", "/admin/login", httpx.Handler(s.login))
	r.Group(func(r chi.Router) {
		r.Use(s.staffOnly)
		r.Method("GET", "/admin/clients", httpx.Handler(s.clients))
		r.Method("GET", "/admin/tenants/{id}", httpx.Handler(s.tenant))
		r.Method("POST", "/admin/tenants/{id}/plan", httpx.Handler(s.changePlan))
		r.Method("POST", "/admin/tenants/{id}/status", httpx.Handler(s.setStatus))
		r.Method("POST", "/admin/tenants/{id}/founder", httpx.Handler(s.setFounder))
		r.Method("POST", "/admin/tenants/{id}/flags", httpx.Handler(s.setFlags))
		r.Method("GET", "/admin/revenue", httpx.Handler(s.revenue))
		r.Method("GET", "/admin/audit", httpx.Handler(s.audit))
		r.Method("GET", "/admin/export.csv", httpx.Handler(s.export))
		r.Method("GET", "/admin/settings/payments", httpx.Handler(s.paymentSettings))
		r.Method("POST", "/admin/settings/payments", httpx.Handler(s.savePaymentSettings))
		r.Method("POST", "/admin/settings/payments/test", httpx.Handler(s.testPaymentKeys))
	})
}

var bearer = regexp.MustCompile(`(?i)^Bearer\s+(.+)$`)

func (s *Service) staffOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := bearer.FindStringSubmatch(r.Header.Get("Authorization"))
		if m == nil {
			httpx.Fail(w, r, httpx.Err(httpx.Unauthorized, "Please sign in."))
			return
		}
		c, err := jwt.Verify(m[1], s.Cfg.AdminTokenSecret)
		if err != nil || c["adm"] != true || c.Str("sub") == "" {
			code := httpx.Unauthorized
			if err == jwt.ErrExpired {
				code = httpx.TokenExpired
			}
			httpx.Fail(w, r, httpx.Err(code, "Please sign in again."))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c.Str("sub"))))
	})
}

func staff(r *http.Request) string { return "admin:" + r.Context().Value(ctxKey{}).(string) }

func (s *Service) log(r *http.Request, action, tenantID string, detail bson.M) {
	if err := privacy.Audit(r.Context(), s.M.AdminDB, staff(r), action, tenantID, detail); err != nil {
		slog.Error("audit_failed", "action", action, "err", err)
	}
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) error {
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	email := strings.ToLower(httpx.Str(b["email"], 200))
	pw, _ := b["password"].(string)
	key := email + "|" + strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]
	if s.limited(key, false) {
		return httpx.Err(httpx.OTPRateLimited, "Too many tries. Wait 15 minutes.")
	}
	if email == "" || !CheckPassword(s.Cfg.AdminUsers, email, pw) {
		s.limited(key, true)
		return httpx.Err(httpx.Unauthorized, "Email or password is not right.")
	}
	tok := jwt.Sign(jwt.Claims{"sub": email, "adm": true}, s.Cfg.AdminTokenSecret, TokenTTL)
	_ = privacy.Audit(r.Context(), s.M.AdminDB, "admin:"+email, "admin_login", "", nil)
	httpx.JSON(w, 200, bson.M{"token": tok, "expires_in": int(TokenTTL.Seconds()), "email": email})
	return nil
}

func (s *Service) clients(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	rows, err := s.clientRows(r.Context(), q.Get("q"), q.Get("plan"), q.Get("status"))
	if err != nil {
		return err
	}
	k, err := s.kpis(r.Context())
	if err != nil {
		return err
	}
	s.log(r, "clients_viewed", "", bson.M{"q": q.Get("q"), "plan": q.Get("plan"), "status": q.Get("status")})
	httpx.JSON(w, 200, bson.M{"kpis": k, "rows": rows})
	return nil
}

func (s *Service) loadTenant(ctx context.Context, id string) (bson.M, error) {
	var t bson.M
	err := s.M.AdminDB.Collection("tenants").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(tenantFields)).Decode(&t)
	if db.NotFound(err) {
		return nil, httpx.Err(httpx.NotFound, "That class was not found.")
	}
	return t, err
}

func (s *Service) tenant(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	t, err := s.loadTenant(ctx, id)
	if err != nil {
		return err
	}
	st := plans.StateOf(t, time.Now())
	tv := bson.M{"id": id, "read_only": st.ReadOnly}
	for k := range tenantFields {
		tv[k] = docs.Plain(t[k])
	}
	if pu, ok := val.Time(t["paid_until"]); ok {
		tv["paid_until"] = val.ISO(pu)
	}
	cur, err := s.M.AdminDB.Collection("invoices").Find(ctx, bson.M{"tenant_id": id}, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(50).
		SetProjection(bson.M{"plan": 1, "billing": 1, "price": 1, "credit": 1, "amount": 1, "status": 1, "provider": 1, "gst_invoice_no": 1, "created_at": 1, "paid_at": 1}))
	if err != nil {
		return err
	}
	var invs []bson.M
	if err := cur.All(ctx, &invs); err != nil {
		return err
	}
	invOut := []any{}
	for _, inv := range invs {
		m := docs.Plain(inv).(bson.M)
		m["id"] = m["_id"]
		delete(m, "_id")
		invOut = append(invOut, m)
	}
	cur, err = s.M.AdminDB.Collection("audit_log").Find(ctx, bson.M{"tenant_id": id}, options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(50))
	if err != nil {
		return err
	}
	var logs []bson.M
	if err := cur.All(ctx, &logs); err != nil {
		return err
	}
	logOut := []bson.M{}
	for _, l := range logs {
		logOut = append(logOut, bson.M{"at": docs.Plain(l["at"]), "actor": l["actor"], "action": l["action"]})
	}
	s.log(r, "tenant_viewed", id, nil)
	httpx.JSON(w, 200, bson.M{"tenant": tv, "plan_def": st.Def, "invoices": invOut, "log": logOut})
	return nil
}

func (s *Service) update(r *http.Request, id string, set bson.M, action string, detail bson.M) error {
	set["updated_at"] = time.Now()
	res, err := s.M.AdminDB.Collection("tenants").UpdateOne(r.Context(), bson.M{"_id": id}, bson.M{"$set": set})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return httpx.Err(httpx.NotFound, "That class was not found.")
	}
	s.log(r, action, id, detail)
	return nil
}

// release puts back messages held for the plan once the plan changes. It
// uses the service's own database user: the admin user cannot touch jobs.
func (s *Service) release(r *http.Request, id string) {
	if _, err := jobs.Release(r.Context(), s.M.DB, bson.M{"tenant_id": id, "hold_reason": bson.M{"$in": bson.A{"plan", "quota"}}}); err != nil {
		slog.Error("release_failed", "err", err)
	}
}

func (s *Service) changePlan(w http.ResponseWriter, r *http.Request) error {
	id := chi.URLParam(r, "id")
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	plan := httpx.Str(b["plan"], 20)
	if _, ok := plans.Plans[plan]; !ok {
		return httpx.Err(httpx.BadRequest, "Unknown plan.")
	}
	until, _ := val.Date(b["until"])
	billing := httpx.Str(b["billing"], 10)
	set := bson.M{"plan": plan}
	if plan == "trial" {
		if until == "" {
			until = time.Now().In(val.IST).AddDate(0, 0, plans.TrialDays).Format("2006-01-02")
		}
		set["trial_ends_on"] = until
	} else {
		pu := time.Now().Add(30 * 24 * time.Hour)
		if until != "" {
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", until+" 23:59:59", val.IST); err == nil {
				pu = t
			}
		}
		set["paid_until"] = pu
		if billing != "yearly" {
			billing = "monthly"
		}
		set["billing"] = billing
	}
	var untilV any
	if until != "" {
		untilV = until
	}
	if err := s.update(r, id, set, "plan_changed", bson.M{"plan": plan, "until": untilV, "reason": httpx.Str(b["reason"], 300)}); err != nil {
		return err
	}
	s.release(r, id)
	httpx.NoContent(w)
	return nil
}

func (s *Service) setStatus(w http.ResponseWriter, r *http.Request) error {
	id := chi.URLParam(r, "id")
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	status, action := "active", "tenant_reactivated"
	if httpx.Str(b["status"], 20) == "suspended" {
		status, action = "suspended", "tenant_suspended"
	}
	if err := s.update(r, id, bson.M{"status": status}, action, bson.M{"reason": httpx.Str(b["reason"], 300)}); err != nil {
		return err
	}
	if status == "active" {
		s.release(r, id)
	}
	httpx.NoContent(w)
	return nil
}

func (s *Service) setFounder(w http.ResponseWriter, r *http.Request) error {
	id := chi.URLParam(r, "id")
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	on := val.Bool(b["on"])
	set := bson.M{"founder": on}
	if on {
		n, err := s.M.AdminDB.Collection("tenants").CountDocuments(r.Context(), bson.M{"founder": true, "_id": bson.M{"$ne": id}})
		if err != nil {
			return err
		}
		if n >= MaxFounder {
			return httpx.Err(httpx.Conflict, fmt.Sprintf("All %d founding-teacher places are taken. Remove one before granting another.", MaxFounder))
		}
		set["plan"] = "pro"
	}
	action := "founder_pro_removed"
	if on {
		action = "founder_pro_granted"
	}
	if err := s.update(r, id, set, action, nil); err != nil {
		return err
	}
	s.release(r, id)
	httpx.NoContent(w)
	return nil
}

func (s *Service) setFlags(w http.ResponseWriter, r *http.Request) error {
	id := chi.URLParam(r, "id")
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	in := val.M(b["flags"])
	flags := bson.M{}
	for _, k := range Features {
		if v, ok := in[k].(bool); ok {
			flags[k] = v
		}
	}
	if err := s.update(r, id, bson.M{"feature_flags": flags}, "feature_flags_set", bson.M{"flags": flags}); err != nil {
		return err
	}
	s.release(r, id)
	httpx.NoContent(w)
	return nil
}

func (s *Service) audit(w http.ResponseWriter, r *http.Request) error {
	cur, err := s.M.AdminDB.Collection("audit_log").Find(r.Context(), bson.M{}, options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(300))
	if err != nil {
		return err
	}
	var rows []bson.M
	if err := cur.All(r.Context(), &rows); err != nil {
		return err
	}
	out := []bson.M{}
	for _, l := range rows {
		out = append(out, bson.M{"id": l["_id"], "at": docs.Plain(l["at"]), "actor": l["actor"], "action": l["action"], "tenant_id": l["tenant_id"], "detail": docs.Plain(l["detail"])})
	}
	httpx.JSON(w, 200, bson.M{"rows": out})
	return nil
}

// cell stops spreadsheet formula injection from class names.
func cell(v any) string {
	s := fmt.Sprint(v)
	if v == nil {
		s = ""
	}
	if s != "" && strings.ContainsAny(s[:1], "=+-@") {
		s = "'" + s
	}
	return s
}

func (s *Service) export(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.clientRows(r.Context(), "", "", "")
	if err != nil {
		return err
	}
	s.log(r, "clients_exported", "", bson.M{"rows": len(rows)})
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dhakshina-desk-clients-%s.csv"`, time.Now().In(val.IST).Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "class", "city", "plan", "status", "renews", "students", "student_limit", "whatsapp_this_month", "whatsapp_limit"})
	for _, x := range rows {
		limit := ""
		if x.StudentLimit != nil {
			limit = fmt.Sprint(*x.StudentLimit)
		}
		_ = cw.Write([]string{x.ID, cell(x.Name), cell(x.City), x.Plan, x.Status, x.Renews, fmt.Sprint(x.Students), limit, fmt.Sprint(x.Msgs), fmt.Sprint(x.MsgLimit)})
	}
	cw.Flush()
	return cw.Error()
}

// NewSalt is used by the admin-hash command.
func NewSalt() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}
