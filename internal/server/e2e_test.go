package server_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/admin"
	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/billing/gateway"
	"github.com/armohalabs/dhakshina-desk-api/internal/config"
	"github.com/armohalabs/dhakshina-desk-api/internal/indexes"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
	"github.com/armohalabs/dhakshina-desk-api/internal/server"
)

// The end-to-end flow needs a MongoDB replica set (transactions). Set
// MONGODB_TEST_URI; the test creates and drops its own database.

type env struct {
	t    *testing.T
	srv  *httptest.Server
	a    *app.App
	mods *server.Modules
}

func setup(t *testing.T) *env {
	uri := os.Getenv("MONGODB_TEST_URI")
	if uri == "" {
		t.Skip("MONGODB_TEST_URI not set")
	}
	name := fmt.Sprintf("dd_test_%d", time.Now().UnixNano())
	ctx := context.Background()
	m, err := db.Connect(ctx, uri, "", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.DB.Drop(ctx); m.Close(ctx) })
	if err := indexes.Ensure(ctx, m.DB); err != nil {
		t.Fatal(err)
	}
	pw := "correct horse battery"
	cfg := &config.Config{
		Env: "test", MongoDB: name, WebBaseURL: "https://web.test", Gateway: "mock",
		JWTSecret: "j", LinkSecret: "l", FieldEncKey: "f", AdminTokenSecret: "a",
		AdminUsers:          "staff@example.com=" + admin.HashPassword(pw, []byte("0123456789abcdef")),
		WhatsAppAppSecret:   "wa-secret",
		WhatsAppVerifyToken: "verify",
	}
	a := app.New(cfg, m)
	mods := server.NewModules(a)
	srv := httptest.NewServer(server.Router(a, mods))
	cfg.APIBaseURL = srv.URL
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, a: a, mods: mods}
}

type resp struct {
	Status int
	Body   map[string]any
	Raw    string
	Header http.Header
}

func (e *env) do(method, path, token string, body any) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Raw: string(raw), Header: res.Header}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (e *env) want(r resp, status int) resp {
	e.t.Helper()
	if r.Status != status {
		e.t.Fatalf("want %d, got %d: %s", status, r.Status, r.Raw)
	}
	return r
}

func m(v any) map[string]any { x, _ := v.(map[string]any); return x }
func l(v any) []any          { x, _ := v.([]any); return x }
func s(v any) string         { x, _ := v.(string); return x }
func n(v any) int64          { x, _ := v.(float64); return int64(x) }

// The income report is staff only, counts only paid invoices and is logged.
func TestRevenueReport(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	teacher := s(e.login("9000000021")["access_token"])
	adm := s(e.want(e.do("POST", "/v1/admin/login", "", map[string]any{"email": "staff@example.com", "password": "correct horse battery"}), 200).Body["token"])
	e.want(e.do("GET", "/v1/admin/revenue", "", nil), 401)
	e.want(e.do("GET", "/v1/admin/revenue", teacher, nil), 401)

	now := time.Now()
	tid := ids.UUIDv7()
	if _, err := e.a.M.C("tenants").InsertOne(ctx, bson.M{"_id": tid, "name": "Paying", "plan": "pro", "status": "active"}); err != nil {
		t.Fatal(err)
	}
	for _, inv := range []bson.M{
		{"status": "paid", "tenant_id": tid, "paid_at": now},
		{"status": "created", "tenant_id": tid, "created_at": now},
		{"status": "paid", "tenant_id": ids.UUIDv7(), "paid_at": now.Add(-40 * 24 * time.Hour)}, // class since erased
	} {
		inv["_id"], inv["plan"], inv["billing"], inv["price"], inv["amount"] = ids.UUIDv7(), "pro", "monthly", int64(24900), int64(24900)
		if _, err := e.a.M.C("invoices").InsertOne(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	r := e.want(e.do("GET", "/v1/admin/revenue?months=6", adm, nil), 200).Body
	if n(r["collected"]) != 49800 || n(r["recurring"]) != 24900 || n(r["paying_classes"]) != 1 {
		t.Fatalf("report: %v", r)
	}
	if len(l(r["months"])) != 6 || len(l(r["recent"])) != 2 || len(l(r["plans"])) != 3 {
		t.Fatalf("report shape: %v", r)
	}
	if s(m(l(r["recent"])[0])["class"]) != "Paying" || s(m(l(r["recent"])[1])["class"]) != "" {
		t.Errorf("recent: %v", r["recent"])
	}
	if got := len(l(e.want(e.do("GET", "/v1/admin/revenue", adm, nil), 200).Body["months"])); got != 12 {
		t.Errorf("default months: %d", got)
	}
	n, err := e.a.M.C("audit_log").CountDocuments(ctx, bson.M{"action": "revenue_viewed"})
	if err != nil || n != 2 {
		t.Errorf("audit rows: %d, %v", n, err)
	}
}

func (e *env) login(phone string) map[string]any {
	st := e.want(e.do("POST", "/v1/auth/otp/start", "", map[string]any{"phone": phone}), 200).Body
	if s(st["dev_code"]) == "" {
		e.t.Fatal("no dev_code outside production")
	}
	bad := e.do("POST", "/v1/auth/otp/verify", "", map[string]any{"request_id": st["request_id"], "code": "000000x"})
	if bad.Status != 400 || s(m(bad.Body["error"])["code"]) != "otp_invalid" {
		e.t.Fatalf("wrong code accepted: %s", bad.Raw)
	}
	return e.want(e.do("POST", "/v1/auth/otp/verify", "", map[string]any{"request_id": st["request_id"], "code": st["dev_code"], "device_id": ids.UUIDv7()}), 200).Body
}

// DELETE /v1/me must really remove the class: only the tax record stays, and
// no other class loses anything.
func TestAccountErasure(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	newClass := func(phone, name string) (string, string, string) {
		v := e.login(phone)
		c := e.want(e.do("POST", "/v1/tenants", s(v["access_token"]), map[string]any{"name": name}), 201).Body
		return s(m(v["user"])["id"]), s(m(c["tenant"])["id"]), s(c["access_token"])
	}
	userID, gone, tok := newClass("9000000011", "Leaving")
	_, kept, keptTok := newClass("9000000012", "Staying")
	if userID == "" || gone == "" || kept == "" {
		t.Fatalf("missing ids: user %q, classes %q %q", userID, gone, kept)
	}

	// One row in every class collection, for both classes. Held jobs are never
	// claimed, so the only job the worker runs below is the erasure.
	tables := []string{"batches", "fee_plans", "guardians", "students", "enrolments", "charges", "adjustments", "payment_claims", "messages", "reminders", "consents", "jobs", "usage_counters", "subscriptions", "payments", "invoices"}
	for _, tid := range []string{gone, kept} {
		for _, c := range tables {
			doc := bson.M{"_id": ids.UUIDv7(), "tenant_id": tid, "note": "about a child"}
			switch c {
			case "guardians":
				doc["phone_hash"] = "only-" + tid
			case "jobs":
				doc["status"] = "held"
			}
			if _, err := e.a.M.C(c).InsertOne(ctx, doc); err != nil {
				t.Fatalf("seed %s: %v", c, err)
			}
		}
		if _, err := e.a.M.C("guardians").InsertOne(ctx, bson.M{"_id": ids.UUIDv7(), "tenant_id": tid, "phone_hash": "shared"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"only-" + gone, "only-" + kept, "shared"} {
		if _, err := e.a.M.C("inbound_messages").InsertOne(ctx, bson.M{"_id": ids.UUIDv7(), "wa_id": h, "from_hash": h}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.a.M.C("data_requests").InsertOne(ctx, bson.M{"_id": ids.UUIDv7(), "tenant_id": gone, "kind": "erase", "status": "open"}); err != nil {
		t.Fatal(err)
	}

	count := func(c string, f bson.M) int64 {
		t.Helper()
		n, err := e.a.M.C(c).CountDocuments(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.want(e.do("DELETE", "/v1/me", tok, nil), 202)
	if _, err := e.mods.Worker.RunOne(ctx); err != nil {
		t.Fatal(err)
	}

	for _, c := range []string{"batches", "fee_plans", "guardians", "students", "enrolments", "charges", "adjustments", "payment_claims", "messages", "reminders", "consents", "jobs", "usage_counters", "subscriptions", "change_log", "memberships"} {
		if n := count(c, bson.M{"tenant_id": gone}); n != 0 {
			t.Errorf("%s: %d rows left for the erased class", c, n)
		}
		if c != "change_log" && count(c, bson.M{"tenant_id": kept}) == 0 {
			t.Errorf("%s: the other class lost its rows", c)
		}
	}
	if count("tenants", bson.M{"_id": gone}) != 0 || count("users", bson.M{"_id": userID}) != 0 {
		t.Error("class or user still there")
	}
	if count("tenants", bson.M{"_id": kept}) != 1 {
		t.Error("the other class was erased")
	}
	// The tax record stays, without its free text.
	if count("payments", bson.M{"tenant_id": gone}) != 1 || count("invoices", bson.M{"tenant_id": gone}) != 1 {
		t.Error("payments and invoices must be kept")
	}
	if count("payments", bson.M{"tenant_id": gone, "note": bson.M{"$exists": true}}) != 0 {
		t.Error("payment note kept")
	}
	if count("payments", bson.M{"tenant_id": kept, "note": bson.M{"$exists": true}}) != 1 {
		t.Error("the other class's payment was changed")
	}
	// A parent also in the other class keeps their messages.
	if count("inbound_messages", bson.M{"from_hash": "only-" + gone}) != 0 {
		t.Error("guardian messages left")
	}
	if count("inbound_messages", bson.M{"from_hash": bson.M{"$in": bson.A{"shared", "only-" + kept}}}) != 2 {
		t.Error("another class's guardian messages removed")
	}
	if count("data_requests", bson.M{"tenant_id": gone, "kind": "erase"}) != 0 {
		t.Error("parent request left")
	}
	if count("data_requests", bson.M{"tenant_id": gone, "kind": "account_erase", "status": "done"}) != 1 {
		t.Error("request not marked done")
	}
	if count("audit_log", bson.M{"tenant_id": gone, "action": "account_erased"}) != 1 {
		t.Error("no audit entry")
	}

	// A token issued before the erasure cannot sync the class back in.
	e.do("POST", "/v1/sync", tok, map[string]any{"since_seq": 0, "changes": []any{
		map[string]any{"table": "batches", "op": "upsert", "id": ids.UUIDv7(), "hlc": hlc("x", 1), "data": map[string]any{"name": "Back again"}},
	}})
	if n := count("batches", bson.M{"tenant_id": gone}); n != 0 {
		t.Errorf("stale token wrote %d rows into the erased class", n)
	}

	// Signing in again is a new account with no class.
	again := e.login("9000000011")
	if s(m(again["user"])["id"]) == userID || len(l(again["memberships"])) != 0 {
		t.Errorf("sign-in after erasure: %v", again)
	}
	e.want(e.do("POST", "/v1/sync", keptTok, map[string]any{}), 200)

	// A request left open from before the job existed still gets carried out.
	v := e.login("9000000013")
	c := e.want(e.do("POST", "/v1/tenants", s(v["access_token"]), map[string]any{"name": "Old request"}), 201).Body
	oldTenant := s(m(c["tenant"])["id"])
	if _, err := e.a.M.C("data_requests").InsertOne(ctx, bson.M{"_id": ids.UUIDv7(), "tenant_id": oldTenant, "user_id": m(v["user"])["id"], "kind": "account_erase", "status": "open"}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second pass must not queue a duplicate
		if err := e.mods.Worker.QueueOpenErasures(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := count("jobs", bson.M{"kind": "erase_account", "status": "queued"}); n != 1 {
		t.Fatalf("erase jobs queued: %d", n)
	}
	if _, err := e.mods.Worker.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if count("tenants", bson.M{"_id": oldTenant}) != 0 {
		t.Error("old open request was not carried out")
	}
}

func hlc(dev string, i int) string {
	return fmt.Sprintf("%s|%04x|%s", val.ISO(time.Now()), i, dev)
}

func findChange(changes []any, table, id string) map[string]any {
	for _, c := range changes {
		if s(m(c)["table"]) == table && s(m(c)["id"]) == id {
			return m(c)
		}
	}
	return nil
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	// Login and class setup.
	v := e.login("+91 98450 12345")
	if len(l(v["memberships"])) != 0 {
		t.Fatal("new user should have no classes")
	}
	tenantID := ids.UUIDv7()
	setup := e.want(e.do("POST", "/v1/tenants", s(v["access_token"]), map[string]any{
		"id": tenantID, "name": "Shree Natya Academy", "owner_name": "Lakshmi", "upi_id": "lakshmi@okhdfc", "upi_name": "Lakshmi R", "city": "Chennai",
	}), 201).Body
	tok := s(setup["access_token"])
	if s(m(setup["tenant"])["receipt_prefix"]) != "DD" || len(l(setup["memberships"])) != 1 {
		t.Fatalf("setup: %v", setup)
	}
	// Retried setup returns the same class.
	e.want(e.do("POST", "/v1/tenants", tok, map[string]any{"id": tenantID, "name": "Shree Natya Academy"}), 201)

	// Refresh rotates; the old refresh token stops working.
	r1 := e.want(e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": setup["refresh_token"]}), 200).Body
	e.want(e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": setup["refresh_token"]}), 401)
	tok = s(r1["access_token"])

	// First sync from a new phone: the class is change 1.
	dev := ids.UUIDv7()
	s0 := e.want(e.do("POST", "/v1/sync", tok, map[string]any{"device_id": dev, "since_seq": 0, "changes": []any{}}), 200).Body
	if n(s0["seq"]) != 1 || findChange(l(s0["changes"]), "tenants", tenantID) == nil {
		t.Fatalf("first pull: %v", s0)
	}
	ent := m(s0["entitlements"])
	if s(ent["plan"]) != "trial" || n(m(ent["limits"])["students"]) != 20 || m(ent["limits"])["batches"] == nil {
		t.Fatalf("entitlements: %v", ent)
	}

	planID, batchID, gID, stID, enID := ids.UUIDv7(), ids.UUIDv7(), ids.UUIDv7(), ids.UUIDv7(), ids.UUIDv7()
	chargeID := ids.CycleChargeID(enID, "2026-09")
	payID, msgID := ids.UUIDv7(), ids.UUIDv7()
	changes := []any{
		map[string]any{"table": "fee_plans", "op": "upsert", "id": planID, "hlc": hlc(dev, 1), "data": map[string]any{"name": "Monthly ₹1,500", "cycle": "monthly", "versions": []any{map[string]any{"amount": 150000, "effective_from": "2026-06-01"}}}},
		map[string]any{"table": "batches", "op": "upsert", "id": batchID, "hlc": hlc(dev, 2), "data": map[string]any{"name": "Bharatanatyam", "schedule": "Sat 9am", "fee_plan_id": planID, "due_day": 5}},
		map[string]any{"table": "guardians", "op": "upsert", "id": gID, "hlc": hlc(dev, 3), "data": map[string]any{"name": "Priya Iyer", "phone": "9876543210"}},
		map[string]any{"table": "students", "op": "insert", "id": stID, "hlc": hlc(dev, 4), "data": map[string]any{"name": "Ananya Iyer", "guardian_id": gID, "dob": "2016-04-02"}},
		map[string]any{"table": "enrolments", "op": "upsert", "id": enID, "hlc": hlc(dev, 5), "data": map[string]any{"student_id": stID, "batch_id": batchID, "fee_plan_id": planID, "joined_on": "2026-06-01"}},
		map[string]any{"table": "charges", "op": "insert", "id": chargeID, "data": map[string]any{"enrolment_id": enID, "cycle": "2026-09", "kind": "fee", "amount": 150000, "due_on": "2026-09-05", "label": "September fee"}},
		map[string]any{"table": "charges", "op": "insert", "id": ids.CycleChargeID(enID, "2026-10"), "data": map[string]any{"enrolment_id": enID, "cycle": "2026-10", "kind": "fee", "amount": 150000, "due_on": "2026-10-05", "label": "October fee"}},
		map[string]any{"table": "payments", "op": "insert", "id": payID, "data": map[string]any{"enrolment_id": enID, "amount": 50000, "mode": "upi", "utr": "123456789012", "paid_on": "2026-09-08"}},
		map[string]any{"table": "messages", "op": "insert", "id": msgID, "data": map[string]any{"kind": "receipt", "guardian_id": gID, "enrolment_id": enID, "payment_id": payID}},
		// Rejections.
		map[string]any{"table": "payments", "op": "insert", "id": ids.UUIDv7(), "data": map[string]any{"enrolment_id": enID, "amount": 100, "mode": "upi", "utr": "123456789012", "paid_on": "2026-09-09"}},
		map[string]any{"table": "charges", "op": "insert", "id": ids.UUIDv7(), "data": map[string]any{"enrolment_id": enID, "cycle": "2026-09", "kind": "fee", "amount": 150000, "due_on": "2026-09-05"}},
		map[string]any{"table": "enrolments", "op": "upsert", "id": ids.UUIDv7(), "data": map[string]any{"student_id": stID, "batch_id": batchID, "fee_plan_id": planID, "joined_on": "2026-07-01"}},
		map[string]any{"table": "payments", "op": "upsert", "id": payID, "data": map[string]any{"amount": 1}},
		map[string]any{"table": "nonsense", "op": "insert", "id": ids.UUIDv7(), "data": map[string]any{}},
		map[string]any{"table": "batches", "op": "upsert", "id": "not-a-uuid"},
	}
	s1 := e.want(e.do("POST", "/v1/sync", tok, map[string]any{"device_id": dev, "since_seq": 1, "changes": changes}), 200).Body
	if len(l(s1["accepted"])) != 9 {
		t.Fatalf("accepted %d: %v", len(l(s1["accepted"])), s1["rejected"])
	}
	codes := map[string]bool{}
	for _, r := range l(s1["rejected"]) {
		codes[s(m(r)["code"])] = true
	}
	for _, c := range []string{"duplicate_utr", "duplicate_charge", "duplicate_enrolment", "bad_request"} {
		if !codes[c] {
			t.Fatalf("missing rejection %s: %v", c, s1["rejected"])
		}
	}
	pay := findChange(l(s1["changes"]), "payments", payID)
	if pay == nil || !strings.HasPrefix(s(m(pay["data"])["receipt_no"]), "DD-") || s(pay["op"]) != "insert" {
		t.Fatalf("payment pull: %v", pay)
	}
	g := findChange(l(s1["changes"]), "guardians", gID)
	if s(m(g["data"])["phone"]) != "+919876543210" || m(g["data"])["phone_enc"] != nil || s(m(g["data"])["consent"]) != "pending" {
		t.Fatalf("guardian pull: %v", g)
	}
	if st := findChange(l(s1["changes"]), "students", stID); s(m(st["data"])["dob"]) != "2016-04-02" {
		t.Fatalf("student dob: %v", st)
	}
	// Stored encrypted, never in the clear.
	var gdoc bson.M
	_ = e.a.M.C("guardians").FindOne(ctx, bson.M{"_id": gID}).Decode(&gdoc)
	if gdoc["phone"] != nil || !strings.HasPrefix(val.Str(gdoc["phone_enc"]), "v1.") || gdoc["tenant_id"] != tenantID {
		t.Fatalf("guardian stored as %v", gdoc)
	}

	// Retrying the same batch is harmless; roster LWW: an older hlc is accepted but not applied.
	seq1 := n(s1["seq"])
	s2 := e.want(e.do("POST", "/v1/sync", tok, map[string]any{"device_id": dev, "since_seq": seq1, "changes": []any{
		changes[7],
		map[string]any{"table": "batches", "op": "upsert", "id": batchID, "hlc": "2020-01-01T00:00:00.000Z|0000|old", "data": map[string]any{"name": "Stale"}},
		map[string]any{"table": "batches", "op": "upsert", "id": batchID, "hlc": hlc(dev, 9), "data": map[string]any{"schedule": "Sun 10am"}},
		map[string]any{"table": "messages", "op": "upsert", "id": msgID, "data": map[string]any{"status": "cancelled"}},
	}}), 200).Body
	if len(l(s2["accepted"])) != 4 || len(l(s2["rejected"])) != 0 {
		t.Fatalf("second push: %v", s2)
	}
	b := m(findChange(l(s2["changes"]), "batches", batchID)["data"])
	if s(b["name"]) != "Bharatanatyam" || s(b["schedule"]) != "Sun 10am" {
		t.Fatalf("partial upsert / LWW: %v", b)
	}
	if mm := findChange(l(s2["changes"]), "messages", msgID); s(m(mm["data"])["status"]) != "cancelled" {
		t.Fatalf("cancel: %v", mm)
	}
	var job bson.M
	_ = e.a.M.C("jobs").FindOne(ctx, bson.M{"ref": msgID}).Decode(&job)
	if job["status"] != "cancelled" {
		t.Fatalf("receipt job not cancelled: %v", job)
	}

	// Gzip request body.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"since_seq":0,"changes":[]}`))
	_ = zw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/sync", &gz)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("gzip sync: %v %v", err, res.StatusCode)
	}
	res.Body.Close()

	// Class view for the website.
	cl := e.want(e.do("GET", "/v1/class", tok, nil), 200).Body
	if len(l(cl["batches"])) != 1 || len(l(cl["fee_plans"])) != 1 || s(cl["role"]) != "owner" || n(m(m(cl["tenant"])["stats"])["students"]) != 1 {
		t.Fatalf("class: %v", cl)
	}

	// Parent page.
	link := e.want(e.do("POST", "/v1/links/parent", tok, map[string]any{"guardian_id": gID}), 200).Body
	ptok := strings.TrimPrefix(s(link["url"]), "https://web.test/p/")
	fam := e.want(e.do("GET", "/v1/parent/"+ptok, "", nil), 200).Body
	fb := m(l(fam["batches"])[0])
	if n(fam["total_due"]) != 250000 || s(fb["state"]) != "overdue" || !strings.Contains(s(fb["note"]), "Sep") || !strings.HasPrefix(s(fb["upi_link"]), "upi://pay?pa=lakshmi%40okhdfc") {
		t.Fatalf("family: %v", fam)
	}
	if len(l(fam["receipts"])) != 1 || s(m(fam["guardian"])["consent"]) != "pending" {
		t.Fatalf("family receipts/consent: %v", fam)
	}
	e.want(e.do("GET", "/v1/parent/"+ptok+"x", "", nil), 404)
	e.want(e.do("POST", "/v1/parent/"+ptok+"/claims", "", map[string]any{"enrolment_id": enID, "utr": "12345"}), 400)
	c1 := e.want(e.do("POST", "/v1/parent/"+ptok+"/claims", "", map[string]any{"enrolment_id": enID, "utr": "999988887777", "app": "gpay"}), 201).Body
	c2 := e.want(e.do("POST", "/v1/parent/"+ptok+"/claims", "", map[string]any{"enrolment_id": enID}), 201).Body
	if c1["duplicate"] != false || c2["duplicate"] != true {
		t.Fatalf("claims: %v %v", c1, c2)
	}
	e.want(e.do("POST", "/v1/parent/"+ptok+"/consent", "", map[string]any{"choice": "given", "ip": "1.2.3.4"}), 204)
	e.want(e.do("POST", "/v1/parent/"+ptok+"/requests", "", map[string]any{"kind": "access"}), 204)
	// Form post without JavaScript gets a 303 back to the page.
	freq, _ := http.NewRequest("POST", e.srv.URL+"/v1/parent/"+ptok+"/requests", strings.NewReader("kind=erase"))
	freq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fres, _ := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(freq)
	if fres.StatusCode != 303 || !strings.HasPrefix(fres.Header.Get("Location"), "https://web.test/p/") {
		t.Fatalf("form post: %d %s", fres.StatusCode, fres.Header.Get("Location"))
	}

	// The phone sees the claim and the consent; confirming upserts status.
	s3 := e.want(e.do("POST", "/v1/sync", tok, map[string]any{"since_seq": n(s2["seq"]), "changes": []any{}}), 200).Body
	claimID := s(c1["id"])
	if cc := findChange(l(s3["changes"]), "payment_claims", claimID); s(m(cc["data"])["status"]) != "pending" || n(m(cc["data"])["amount"]) != 250000 {
		t.Fatalf("claim pull: %v", cc)
	}
	if gg := findChange(l(s3["changes"]), "guardians", gID); s(m(gg["data"])["consent"]) != "given" {
		t.Fatalf("consent pull: %v", gg)
	}
	s4 := e.want(e.do("POST", "/v1/sync", tok, map[string]any{"since_seq": n(s3["seq"]), "changes": []any{
		map[string]any{"table": "payment_claims", "op": "upsert", "id": claimID, "hlc": hlc(dev, 20), "data": map[string]any{"status": "confirmed"}},
		map[string]any{"table": "payments", "op": "insert", "id": ids.UUIDv7(), "data": map[string]any{"enrolment_id": enID, "amount": 250000, "mode": "upi", "utr": "999988887777", "paid_on": "2026-10-24", "claim_id": claimID}},
		map[string]any{"table": "guardians", "op": "upsert", "id": gID, "hlc": hlc(dev, 21), "data": map[string]any{"consent": "withdrawn"}},
	}}), 200).Body
	if len(l(s4["accepted"])) != 3 {
		t.Fatalf("confirm: %v", s4)
	}
	_ = e.a.M.C("guardians").FindOne(ctx, bson.M{"_id": gID}).Decode(&gdoc)
	if gdoc["consent"] != "given" {
		t.Fatal("the app must not be able to set consent")
	}

	// Second phone: a co-device of the same teacher pulls everything from 0.
	full := e.want(e.do("POST", "/v1/sync", tok, map[string]any{"since_seq": 0}), 200).Body
	if findChange(l(full["changes"]), "enrolments", enID) == nil || findChange(l(full["changes"]), "tenants", tenantID) == nil {
		t.Fatal("full pull incomplete")
	}

	// Plan limit: trial allows 3 batches.
	var lim []any
	for i := 0; i < 3; i++ {
		lim = append(lim, map[string]any{"table": "batches", "op": "upsert", "id": ids.UUIDv7(), "data": map[string]any{"name": fmt.Sprint("B", i)}})
	}
	sl := e.want(e.do("POST", "/v1/sync", tok, map[string]any{"since_seq": 0, "changes": lim}), 200).Body
	if len(l(sl["rejected"])) != 1 || s(m(l(sl["rejected"])[0])["code"]) != "plan_limit" {
		t.Fatalf("plan limit: %v", sl["rejected"])
	}

	// Upgrade link, quote, mock checkout, webhook-free mock pay.
	up := e.want(e.do("POST", "/v1/links/upgrade", tok, nil), 200).Body
	utok := s(up["url"])[len("https://web.test/subscribe/"):]
	q := e.want(e.do("GET", "/v1/billing/quote?plan=pro&billing=yearly&token="+utok, "", nil), 200).Body
	if n(m(q["quote"])["amount"]) != 249900 || m(q["plans"])["pro"] == nil || m(q["tenant"])["students"] == nil {
		t.Fatalf("quote: %v", q)
	}
	e.want(e.do("GET", "/v1/billing/quote?token=bad", "", nil), 401)
	co := e.want(e.do("POST", "/v1/billing/checkout", "", map[string]any{"token": utok, "plan": "pro", "billing": "monthly"}), 201).Body
	inv := s(co["invoice_id"])
	if s(m(co["client"])["provider"]) != "mock" {
		t.Fatalf("checkout: %v", co)
	}
	e.want(e.do("GET", "/v1/billing/invoices/"+inv, "", nil), 200)
	mp := e.do("GET", strings.TrimPrefix(s(m(co["client"])["url"]), e.srv.URL), "", nil)
	if mp.Status != 303 || mp.Header.Get("Location") != "https://web.test/subscribe/done?invoice="+inv {
		t.Fatalf("mock pay: %d %s", mp.Status, mp.Header.Get("Location"))
	}
	e.want(e.do("GET", strings.TrimPrefix(s(m(co["client"])["url"]), e.srv.URL), "", nil), 303) // idempotent
	iv := e.want(e.do("GET", "/v1/billing/invoices/"+inv, "", nil), 200).Body
	if s(iv["status"]) != "paid" || !strings.HasPrefix(s(iv["gst_invoice_no"]), "DDI-") {
		t.Fatalf("invoice: %v", iv)
	}
	sp := e.want(e.do("POST", "/v1/sync", tok, map[string]any{"since_seq": n(sl["seq"])}), 200).Body
	if s(m(sp["entitlements"])["plan"]) != "pro" || m(m(sp["entitlements"])["limits"])["batches"] != nil {
		t.Fatalf("after payment: %v", sp["entitlements"])
	}
	if findChange(l(sp["changes"]), "tenants", tenantID) == nil {
		t.Fatal("plan change not logged for sync")
	}
	// Signed webhooks with a bad signature are ignored.
	e.want(e.do("POST", "/v1/billing/webhook/razorpay", "", map[string]any{"event": "order.paid"}), 200)

	// Worker: a reminder is sent on Pro (outside quiet hours) and its
	// delivery status comes back through the webhook.
	w := e.mods.Worker
	w.Now = func() time.Time { return time.Date(2026, 10, 24, 11, 0, 0, 0, val.IST) }
	remID := ids.UUIDv7()
	e.want(e.do("POST", "/v1/sync", tok, map[string]any{"changes": []any{
		map[string]any{"table": "charges", "op": "insert", "id": ids.UUIDv7(), "data": map[string]any{"enrolment_id": enID, "cycle": "2026-11", "kind": "fee", "amount": 150000, "due_on": "2026-11-05"}},
		map[string]any{"table": "messages", "op": "insert", "id": remID, "data": map[string]any{"kind": "reminder", "guardian_id": gID, "enrolment_id": enID}},
	}}), 200)
	for i := 0; i < 5; i++ {
		if _, err := w.RunOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var msg bson.M
	_ = e.a.M.C("messages").FindOne(ctx, bson.M{"_id": remID}).Decode(&msg)
	if msg["status"] != "sent" || val.Str(msg["wa_id"]) == "" {
		t.Fatalf("reminder not sent: %v", msg)
	}
	hook := fmt.Sprintf(`{"entry":[{"changes":[{"value":{"statuses":[{"id":%q,"status":"delivered"}]}}]}]}`, msg["wa_id"])
	mac := hmac.New(sha256.New, []byte("wa-secret"))
	mac.Write([]byte(hook))
	hreq, _ := http.NewRequest("POST", e.srv.URL+"/v1/webhooks/whatsapp", strings.NewReader(hook))
	hreq.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	hres, _ := http.DefaultClient.Do(hreq)
	if hres.StatusCode != 200 {
		t.Fatalf("webhook: %d", hres.StatusCode)
	}
	for i := 0; i < 3; i++ {
		_, _ = w.RunOne(ctx)
	}
	_ = e.a.M.C("messages").FindOne(ctx, bson.M{"_id": remID}).Decode(&msg)
	if msg["status"] != "delivered" {
		t.Fatalf("status not applied: %v", msg["status"])
	}
	bad, _ := http.Post(e.srv.URL+"/v1/webhooks/whatsapp", "application/json", strings.NewReader(hook))
	if bad.StatusCode != 401 {
		t.Fatalf("unsigned webhook accepted: %d", bad.StatusCode)
	}

	// Daily scheduler queues automatic reminders once per stage.
	_, _ = e.a.M.C("reminders").DeleteMany(ctx, bson.M{})
	day := time.Date(2026, 11, 5, 7, 30, 0, 0, val.IST)
	w.Now = func() time.Time { return day }
	if err := w.DailyPass(ctx, day); err != nil {
		t.Fatal(err)
	}
	if err := w.DailyPass(ctx, day); err != nil { // second run the same day is a no-op
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, _ = w.RunOne(ctx)
	}
	nr, _ := e.a.M.C("reminders").CountDocuments(ctx, bson.M{"tenant_id": tenantID})
	if nr != 1 {
		t.Fatalf("want 1 scheduled reminder, got %d", nr)
	}

	// Admin: login, clients, tenant view with no class data, controls, export.
	e.want(e.do("POST", "/v1/admin/login", "", map[string]any{"email": "staff@example.com", "password": "wrong"}), 401)
	adm := s(e.want(e.do("POST", "/v1/admin/login", "", map[string]any{"email": "staff@example.com", "password": "correct horse battery"}), 200).Body["token"])
	e.want(e.do("GET", "/v1/admin/clients", tok, nil), 401) // a teacher token is not a staff token
	e.want(e.do("GET", "/v1/admin/clients", "", nil), 401)
	cs := e.want(e.do("GET", "/v1/admin/clients?status=Active", adm, nil), 200).Body
	if len(l(cs["rows"])) != 1 || n(m(cs["kpis"])["total"]) != 1 || n(m(l(cs["rows"])[0])["students"]) != 1 {
		t.Fatalf("clients: %v", cs)
	}
	td := e.want(e.do("GET", "/v1/admin/tenants/"+tenantID, adm, nil), 200)
	for _, leak := range []string{"Ananya", "Priya", "upi_id", "phone"} {
		if strings.Contains(td.Raw, leak) {
			t.Fatalf("admin tenant view leaks %q: %s", leak, td.Raw)
		}
	}
	if len(l(td.Body["invoices"])) != 1 || len(l(td.Body["log"])) == 0 {
		t.Fatalf("tenant view: %v", td.Body)
	}
	e.want(e.do("POST", "/v1/admin/tenants/"+tenantID+"/flags", adm, map[string]any{"flags": map[string]any{"export": false, "junk": true}}), 204)
	e.want(e.do("POST", "/v1/admin/tenants/"+tenantID+"/founder", adm, map[string]any{"on": true}), 204)
	e.want(e.do("POST", "/v1/admin/tenants/"+tenantID+"/status", adm, map[string]any{"status": "suspended", "reason": "test"}), 204)
	e.want(e.do("GET", "/v1/parent/"+ptok, "", nil), 404) // suspended class: page closes
	e.want(e.do("POST", "/v1/admin/tenants/"+tenantID+"/status", adm, map[string]any{"status": "active"}), 204)
	e.want(e.do("POST", "/v1/admin/tenants/"+tenantID+"/plan", adm, map[string]any{"plan": "academy", "billing": "yearly", "until": "2027-10-24"}), 204)
	ex := e.want(e.do("GET", "/v1/admin/export.csv", adm, nil), 200)
	if !strings.Contains(ex.Raw, "Shree Natya Academy") || !strings.HasPrefix(ex.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("export: %s", ex.Raw)
	}
	au := e.want(e.do("GET", "/v1/admin/audit", adm, nil), 200).Body
	if len(l(au["rows"])) < 6 {
		t.Fatalf("audit rows: %d", len(l(au["rows"])))
	}
	// Payment settings: environment first, then saved from the console with
	// secrets encrypted and never shown back.
	ps := e.want(e.do("GET", "/v1/admin/settings/payments", adm, nil), 200).Body
	if s(ps["source"]) != "env" || s(ps["active"]) != "mock" || s(m(ps["razorpay"])["webhook_url"]) == "" {
		t.Fatalf("payment settings: %v", ps)
	}
	e.want(e.do("GET", "/v1/admin/settings/payments", tok, nil), 401)
	e.want(e.do("POST", "/v1/admin/settings/payments", adm, map[string]any{"active": "razorpay", "razorpay": map[string]any{"key_id": "rzp_test_abc123def456"}}), 400)
	e.want(e.do("POST", "/v1/admin/settings/payments", adm, map[string]any{"active": "razorpay", "razorpay": map[string]any{"key_id": "pk_live_x", "key_secret": "s", "webhook_secret": "w"}}), 400)
	const rpSecret, rpHook = "razorpay-key-secret-9876", "razorpay-webhook-secret-5432"
	e.want(e.do("POST", "/v1/admin/settings/payments", adm, map[string]any{"active": "razorpay", "razorpay": map[string]any{
		"key_id": "rzp_test_abc123def456", "key_secret": rpSecret, "webhook_secret": rpHook}}), 204)
	pr := e.want(e.do("GET", "/v1/admin/settings/payments", adm, nil), 200)
	if strings.Contains(pr.Raw, rpSecret) || strings.Contains(pr.Raw, rpHook) || s(m(pr.Body["razorpay"])["key_secret"]) != "••••9876" ||
		s(pr.Body["source"]) != "admin" || s(pr.Body["active"]) != "razorpay" || m(pr.Body["razorpay"])["live"] != false {
		t.Fatalf("saved payment settings: %s", pr.Raw)
	}
	var sdoc bson.M
	_ = e.a.M.DB.Collection("platform_settings").FindOne(ctx, bson.M{"_id": "payment_gateway"}).Decode(&sdoc)
	if raw, _ := bson.MarshalExtJSON(sdoc, false, false); strings.Contains(string(raw), rpSecret) || strings.Contains(string(raw), rpHook) {
		t.Fatalf("secrets stored in plain text: %s", raw)
	}
	// A blank secret keeps the saved one; switching back keeps Razorpay's keys.
	e.want(e.do("POST", "/v1/admin/settings/payments", adm, map[string]any{"active": "mock", "razorpay": map[string]any{"key_id": "rzp_test_abc123def456", "key_secret": ""}}), 204)
	if set, _ := gateway.Load(ctx, e.a.M.DB, e.a.Box, e.a.Cfg); set.Active != "mock" || set.RazorpayKeySecret != rpSecret || set.RazorpayWebhookSecret != rpHook {
		t.Fatalf("settings after switch: %+v", set.Active)
	}
	e.want(e.do("POST", "/v1/admin/settings/payments/test", adm, map[string]any{"provider": "paypal"}), 400)
	au2 := e.want(e.do("GET", "/v1/admin/audit", adm, nil), 200)
	if !strings.Contains(au2.Raw, "payment_settings_changed") || strings.Contains(au2.Raw, rpSecret) {
		t.Fatalf("payment settings audit: %s", au2.Raw)
	}

	sx := e.want(e.do("POST", "/v1/sync", tok, map[string]any{}), 200).Body
	if s(m(sx["entitlements"])["plan"]) != "academy" || m(m(sx["entitlements"])["features"])["export"] != false {
		t.Fatalf("admin changes not in entitlements: %v", sx["entitlements"])
	}

	// Tenant isolation: another teacher cannot see or touch this class.
	v2 := e.login("9000000001")
	other := e.want(e.do("POST", "/v1/tenants", s(v2["access_token"]), map[string]any{"name": "Other Class"}), 201).Body
	otok := s(other["access_token"])
	ox := e.want(e.do("POST", "/v1/sync", otok, map[string]any{"since_seq": 0, "changes": []any{
		map[string]any{"table": "tenants", "op": "upsert", "id": tenantID, "data": map[string]any{"name": "Hijacked"}},
		map[string]any{"table": "batches", "op": "upsert", "id": batchID, "hlc": hlc("x", 99), "data": map[string]any{"name": "Mine now"}},
	}}), 200).Body
	if len(l(ox["changes"])) != 1 || s(m(l(ox["rejected"])[0])["code"]) != "forbidden" {
		t.Fatalf("isolation: %v", ox)
	}
	var bdoc bson.M
	_ = e.a.M.C("batches").FindOne(ctx, bson.M{"_id": batchID, "tenant_id": tenantID}).Decode(&bdoc)
	if bdoc["name"] != "Bharatanatyam" {
		t.Fatalf("cross-tenant write changed the batch: %v", bdoc["name"])
	}
	e.want(e.do("POST", "/v1/links/parent", otok, map[string]any{"guardian_id": gID}), 404)
	e.want(e.do("POST", "/v1/auth/switch", otok, map[string]any{"tenant_id": tenantID}), 403)

	// Account deletion.
	e.want(e.do("DELETE", "/v1/me", otok, nil), 202)
	e.want(e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": other["refresh_token"]}), 401)
}
