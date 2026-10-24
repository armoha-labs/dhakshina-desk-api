// Package admin is the platform console's API. It reads only tenants,
// subscriptions, invoices, usage_counters and audit_log, through the admin
// database user when MONGODB_ADMIN_URI is set, so the database itself
// refuses students, guardians, ledgers and messages. Student counts come
// from tenants.stats. Every call is written to the audit log.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/crypto/scrypt"

	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

const (
	TokenTTL   = 8 * time.Hour
	MaxFounder = 30 // lifetime founder Pro is capped (business model guardrail)
)

var Features = []string{"reminders", "bulk_import", "ocr", "export", "late_fees", "statement_import"}

type Service struct {
	*app.App
	mu       sync.Mutex
	failures map[string][]time.Time
}

func New(a *app.App) *Service { return &Service{App: a, failures: map[string][]time.Time{}} }

var b64 = base64.RawURLEncoding

// HashPassword makes an ADMIN_USERS entry value: scrypt$salt$hash. The salt
// string itself is the scrypt salt, matching the Node implementation, so
// hashes made with the website's old script still work.
func HashPassword(pw string, salt []byte) string {
	s := b64.EncodeToString(salt)
	k, _ := scrypt.Key([]byte(pw), []byte(s), 16384, 8, 1, 32)
	return "scrypt$" + s + "$" + b64.EncodeToString(k)
}

// CheckPassword checks email and password against ADMIN_USERS.
func CheckPassword(users, email, pw string) bool {
	for _, e := range strings.Split(users, ",") {
		e = strings.TrimSpace(e)
		i := strings.Index(e, "=")
		if i < 1 || !strings.EqualFold(e[:i], email) {
			continue
		}
		p := strings.Split(e[i+1:], "$")
		if len(p) != 3 || p[0] != "scrypt" || p[1] == "" || p[2] == "" {
			return false
		}
		want, err := b64.DecodeString(p[2])
		if err != nil {
			return false
		}
		got, err := scrypt.Key([]byte(pw), []byte(p[1]), 16384, 8, 1, len(want))
		return err == nil && subtle.ConstantTimeCompare(got, want) == 1
	}
	return false
}

// limited reports too many failed logins for this key in 15 minutes.
func (s *Service) limited(key string, fail bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-15 * time.Minute)
	var keep []time.Time
	for _, t := range s.failures[key] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if fail {
		keep = append(keep, time.Now())
	}
	s.failures[key] = keep
	return len(keep) >= 10
}

type ClientRow struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	City         string `json:"city"`
	Initials     string `json:"initials"`
	Plan         string `json:"plan"`
	Status       string `json:"status"`
	Renews       string `json:"renews"`
	Students     int64  `json:"students"`
	StudentLimit *int64 `json:"student_limit"`
	Msgs         int64  `json:"msgs"`
	MsgLimit     int64  `json:"msg_limit"`
}

var tenantFields = bson.M{"name": 1, "city": 1, "owner_name": 1, "plan": 1, "billing": 1, "status": 1, "founder": 1,
	"trial_ends_on": 1, "paid_until": 1, "stats": 1, "feature_flags": 1, "created_at": 1}

func statusLabel(t bson.M, st plans.State) string {
	switch {
	case val.Str(t["status"]) == "suspended":
		return "Suspended"
	case st.ReadOnly:
		return "Read only"
	case val.Bool(t["founder"]):
		return "Founder"
	case st.Plan == "trial":
		return "Trial"
	}
	return "Active"
}

func initials(name string) string {
	var out string
	for i, w := range strings.Fields(name) {
		if i == 2 {
			break
		}
		out += strings.ToUpper(string([]rune(w)[:1]))
	}
	return out
}

func (s *Service) clientRows(ctx context.Context, q, plan, status string) ([]ClientRow, error) {
	d := s.M.AdminDB
	f := bson.M{}
	if q != "" {
		if len(q) > 60 {
			q = q[:60]
		}
		f["name"] = bson.M{"$regex": regexp.QuoteMeta(q), "$options": "i"}
	}
	if _, ok := plans.Plans[plan]; ok {
		f["plan"] = plan
	}
	cur, err := d.Collection("tenants").Find(ctx, f, options.Find().SetProjection(tenantFields).SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(500))
	if err != nil {
		return nil, err
	}
	var ts []bson.M
	if err := cur.All(ctx, &ts); err != nil {
		return nil, err
	}
	tids := make([]string, len(ts))
	for i, t := range ts {
		tids[i] = val.Str(t["_id"])
	}
	now := time.Now()
	cur, err = d.Collection("usage_counters").Find(ctx, bson.M{"tenant_id": bson.M{"$in": tids}, "month": plans.MonthKey(now)})
	if err != nil {
		return nil, err
	}
	var us []bson.M
	if err := cur.All(ctx, &us); err != nil {
		return nil, err
	}
	msgs := map[string]int64{}
	for _, u := range us {
		msgs[val.Str(u["tenant_id"])] = val.IntOr(u["whatsapp"], 0)
	}
	rows := []ClientRow{}
	for _, t := range ts {
		st := plans.StateOf(t, now)
		renews := "—"
		switch {
		case st.Plan == "trial":
			ends := val.Str(t["trial_ends_on"])
			if ends == "" {
				ends = "?"
			}
			renews = "Trial ends " + ends
		case t["paid_until"] != nil:
			if pu, ok := val.Time(t["paid_until"]); ok {
				renews = pu.In(val.IST).Format("2006-01-02")
			}
		case val.Bool(t["founder"]):
			renews = "Lifetime"
		}
		row := ClientRow{
			ID: val.Str(t["_id"]), Name: val.Str(t["name"]), City: val.Str(t["city"]), Initials: initials(val.Str(t["name"])),
			Plan: st.Def.Name, Status: statusLabel(t, st), Renews: renews,
			Students: val.IntOr(val.M(t["stats"])["students"], 0), StudentLimit: st.Def.Limits.Students,
			Msgs: msgs[val.Str(t["_id"])], MsgLimit: st.Def.Limits.WhatsAppPerMonth,
		}
		if status == "" || row.Status == status {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (s *Service) kpis(ctx context.Context) (bson.M, error) {
	c := s.M.AdminDB.Collection("tenants")
	now := time.Now()
	today := now.In(val.IST).Format("2006-01-02")
	week := now.In(val.IST).AddDate(0, 0, 7).Format("2006-01-02")
	out := bson.M{}
	for k, f := range map[string]bson.M{
		"total":       {},
		"paying":      {"plan": bson.M{"$ne": "trial"}, "paid_until": bson.M{"$gt": now}},
		"trials":      {"plan": "trial", "trial_ends_on": bson.M{"$gte": today}},
		"ending_soon": {"plan": "trial", "trial_ends_on": bson.M{"$gte": today, "$lte": week}},
		"suspended":   {"status": "suspended"},
	} {
		n, err := c.CountDocuments(ctx, f)
		if err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, nil
}
