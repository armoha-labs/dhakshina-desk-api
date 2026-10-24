// Package collections is the parent side: the family page read model, "I've
// paid" claims, consent and data-rights requests, and the signed family link.
package collections

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/changelog"
	"github.com/armohalabs/dhakshina-desk-api/internal/jobs"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/store"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/upi"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

const ConsentNoticeVersion = "2026-10-en-1"

type Service struct{ *app.App }

type PendingClaim struct {
	Amount    int64   `json:"amount"`
	UTR       *string `json:"utr"`
	ClaimedAt string  `json:"claimed_at"`
}

type FamilyBatch struct {
	EnrolmentID  string        `json:"enrolment_id"`
	Student      string        `json:"student"`
	Batch        string        `json:"batch"`
	Schedule     string        `json:"schedule"`
	Due          int64         `json:"due"`
	State        string        `json:"state"` // overdue, due, paid, advance
	Note         string        `json:"note"`
	UPILink      *string       `json:"upi_link"`
	PendingClaim *PendingClaim `json:"pending_claim"`
	// OpenCycle is the oldest open cycle, used for reminder text; not sent.
	OpenCycle string `json:"-"`
}

type Receipt struct {
	ID        string `json:"id"`
	ReceiptNo string `json:"receipt_no"`
	Amount    int64  `json:"amount"`
	PaidOn    string `json:"paid_on"`
	Batch     string `json:"batch"`
	Mode      string `json:"mode"`
}

type Family struct {
	Tenant struct {
		ID               string  `json:"id"`
		Name             string  `json:"name"`
		UPIID            string  `json:"upi_id"`
		UPIName          string  `json:"upi_name"`
		GrievanceContact *string `json:"grievance_contact"`
		Initials         string  `json:"initials"`
	} `json:"tenant"`
	Guardian struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Consent string `json:"consent"`
	} `json:"guardian"`
	TotalDue int64         `json:"total_due"`
	Batches  []FamilyBatch `json:"batches"`
	Receipts []Receipt     `json:"receipts"`
}

var months = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
var monthCycle = regexp.MustCompile(`^(\d{4})-(\d{2})$`)

func cycleLabel(c string) string {
	if m := monthCycle.FindStringSubmatch(c); m != nil {
		var n int
		fmt.Sscanf(m[2], "%d", &n)
		if n >= 1 && n <= 12 {
			return months[n-1]
		}
	}
	if c == "opening" {
		return "earlier dues"
	}
	return c
}

func initials(name string) string {
	var out []string
	for _, w := range strings.Fields(name) {
		if len(out) == 2 {
			break
		}
		out = append(out, strings.ToUpper(string([]rune(w)[:1])))
	}
	if len(out) == 0 {
		return "DD"
	}
	return strings.Join(out, "")
}

// rupees formats paise in the Indian style, e.g. ₹1,50,000 or ₹1,500.50.
func Rupees(paise int64) string {
	neg := paise < 0
	if neg {
		paise = -paise
	}
	r, p := paise/100, paise%100
	s := fmt.Sprint(r)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		s = strings.Join(parts, ",") + "," + tail
	}
	if p != 0 {
		s += fmt.Sprintf(".%02d", p)
	}
	if neg {
		return "-₹" + s
	}
	return "₹" + s
}

func strs(list []bson.M, k string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range list {
		v := val.Str(d[k])
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// LoadFamily builds the family page: dues per enrolment, oldest open charges
// first. Returns nil when the link no longer points at a live family.
func (s *Service) LoadFamily(ctx context.Context, tenantID, guardianID string) (*Family, error) {
	sc := store.For(s.M.DB, tenantID)
	t, err := sc.Tenant(ctx)
	if err != nil {
		return nil, err
	}
	g, err := sc.FindOne(ctx, "guardians", bson.M{"_id": guardianID, "deleted": bson.M{"$ne": true}})
	if err != nil {
		return nil, err
	}
	if t == nil || g == nil || val.Str(t["status"]) == "suspended" {
		return nil, nil
	}
	live := bson.M{"$ne": true}
	students, err := sc.Find(ctx, "students", bson.M{"guardian_id": guardianID, "deleted": live})
	if err != nil {
		return nil, err
	}
	studentName := map[string]string{}
	for _, st := range students {
		studentName[val.Str(st["_id"])] = val.Str(st["name"])
	}
	enrolments, err := sc.Find(ctx, "enrolments", bson.M{"student_id": bson.M{"$in": strs(students, "_id")}, "deleted": live})
	if err != nil {
		return nil, err
	}
	eIDs := strs(enrolments, "_id")
	in := bson.M{"$in": eIDs}
	batches, err := sc.Find(ctx, "batches", bson.M{"_id": bson.M{"$in": strs(enrolments, "batch_id")}})
	if err != nil {
		return nil, err
	}
	charges, err := sc.Find(ctx, "charges", bson.M{"enrolment_id": in})
	if err != nil {
		return nil, err
	}
	payments, err := sc.Find(ctx, "payments", bson.M{"enrolment_id": in}, options.Find().SetSort(bson.D{{Key: "paid_on", Value: -1}}))
	if err != nil {
		return nil, err
	}
	adjustments, err := sc.Find(ctx, "adjustments", bson.M{"enrolment_id": in})
	if err != nil {
		return nil, err
	}
	claims, err := sc.Find(ctx, "payment_claims", bson.M{"enrolment_id": in, "status": "pending"}, options.Find().SetSort(bson.D{{Key: "claimed_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	batchOf := map[string]bson.M{}
	for _, b := range batches {
		batchOf[val.Str(b["_id"])] = b
	}
	group := func(rows []bson.M) map[string][]bson.M {
		m := map[string][]bson.M{}
		for _, r := range rows {
			k := val.Str(r["enrolment_id"])
			m[k] = append(m[k], r)
		}
		return m
	}
	sum := func(rows []bson.M) int64 {
		var n int64
		for _, r := range rows {
			n += val.IntOr(r["amount"], 0)
		}
		return n
	}
	cBy, pBy, aBy, clBy := group(charges), group(payments), group(adjustments), group(claims)
	today := time.Now().In(val.IST).Format("2006-01-02")
	upiName := val.Str(t["upi_name"])
	if upiName == "" {
		upiName = val.Str(t["name"])
	}

	f := &Family{Batches: []FamilyBatch{}, Receipts: []Receipt{}}
	for _, e := range enrolments {
		id := val.Str(e["_id"])
		cs := cBy[id]
		sort.SliceStable(cs, func(i, j int) bool { return val.Str(cs[i]["due_on"]) < val.Str(cs[j]["due_on"]) })
		paid := sum(pBy[id]) + sum(aBy[id])
		due := sum(cs) - paid
		if val.Str(e["status"]) == "left" && due <= 0 {
			continue
		}
		b := batchOf[val.Str(e["batch_id"])]
		// Which charges are still open: settle the oldest first with what was paid.
		credit := paid
		var open []bson.M
		for _, c := range cs {
			amt := val.IntOr(c["amount"], 0)
			if credit >= amt {
				credit -= amt
			} else {
				open = append(open, c)
				credit = 0
			}
		}
		var overdue []bson.M
		for _, c := range open {
			if val.Str(c["due_on"]) < today {
				overdue = append(overdue, c)
			}
		}
		state := "paid"
		switch {
		case due > 0 && len(overdue) > 0:
			state = "overdue"
		case due > 0:
			state = "due"
		case due < 0:
			state = "advance"
		}
		label := open
		if len(overdue) > 0 {
			label = overdue
		}
		var names []string
		for _, c := range label {
			names = append(names, cycleLabel(val.Str(c["cycle"])))
		}
		joined := strings.Join(names, " and ")
		var note string
		switch state {
		case "overdue":
			note = "Overdue · " + joined
		case "due":
			if joined == "" {
				joined = "this cycle"
			}
			note = "Due · " + joined
		case "advance":
			note = "Paid ahead · " + Rupees(-due) + " credit"
		default:
			note = "All paid"
		}
		cycle := "due"
		if len(open) > 0 {
			cycle = val.Str(open[0]["cycle"])
		}
		student := studentName[val.Str(e["student_id"])]
		batchName := val.Str(b["name"])
		if batchName == "" {
			batchName = "Batch"
		}
		fb := FamilyBatch{
			EnrolmentID: id, Student: student, Batch: batchName, Schedule: val.Str(b["schedule"]),
			Due: max(due, 0), State: state, Note: note, OpenCycle: cycle,
		}
		if due > 0 && val.Str(t["upi_id"]) != "" {
			l := upi.Link(upi.Pay{VPA: val.Str(t["upi_id"]), Payee: upiName, AmountPaise: due, Ref: upi.InvoiceRef(id, cycle),
				Note: strings.TrimSpace(student + " " + val.Str(b["name"]))})
			fb.UPILink = &l
		}
		if cl := clBy[id]; len(cl) > 0 {
			pc := &PendingClaim{Amount: val.IntOr(cl[0]["amount"], 0)}
			if u := val.Str(cl[0]["utr"]); u != "" {
				pc.UTR = &u
			}
			if at, ok := val.Time(cl[0]["claimed_at"]); ok {
				pc.ClaimedAt = val.ISO(at)
			}
			fb.PendingClaim = pc
		}
		f.Batches = append(f.Batches, fb)
	}
	order := map[string]int{"overdue": 0, "due": 1, "advance": 2, "paid": 3}
	sort.SliceStable(f.Batches, func(i, j int) bool {
		a, b := f.Batches[i], f.Batches[j]
		if order[a.State] != order[b.State] {
			return order[a.State] < order[b.State]
		}
		return a.Student < b.Student
	})

	f.Tenant.ID = tenantID
	f.Tenant.Name = val.Str(t["name"])
	f.Tenant.UPIID = val.Str(t["upi_id"])
	f.Tenant.UPIName = upiName
	if gc := val.Str(t["grievance_contact"]); gc != "" {
		f.Tenant.GrievanceContact = &gc
	}
	f.Tenant.Initials = initials(f.Tenant.Name)
	f.Guardian.ID = guardianID
	f.Guardian.Name = val.Str(g["name"])
	f.Guardian.Consent = val.Str(g["consent"])
	if f.Guardian.Consent == "" {
		f.Guardian.Consent = "pending"
	}
	for _, b := range f.Batches {
		f.TotalDue += b.Due
	}
	enrolBatch := map[string]string{}
	for _, e := range enrolments {
		enrolBatch[val.Str(e["_id"])] = val.Str(batchOf[val.Str(e["batch_id"])]["name"])
	}
	for _, p := range payments {
		if len(f.Receipts) == 10 {
			break
		}
		if rn := val.Str(p["receipt_no"]); rn != "" {
			f.Receipts = append(f.Receipts, Receipt{ID: val.Str(p["_id"]), ReceiptNo: rn, Amount: val.IntOr(p["amount"], 0),
				PaidOn: val.Str(p["paid_on"]), Batch: enrolBatch[val.Str(p["enrolment_id"])], Mode: val.Str(p["mode"])})
		}
	}
	return f, nil
}

var utrRE = regexp.MustCompile(`^\d{12}$`)
var apps = map[string]bool{"gpay": true, "phonepe": true, "paytm": true, "other": true}

// CreateClaim is the parent's "I've paid": stored in payment_claims with the
// contract's shape and logged so it reaches the teacher's phone on its next sync.
func (s *Service) CreateClaim(ctx context.Context, tenantID, guardianID, enrolmentID, utr, app string) (string, bool, error) {
	utr = strings.Join(strings.Fields(utr), "")
	if utr != "" && !utrRE.MatchString(utr) {
		return "", false, httpx.Err(httpx.BadRequest, "The UPI reference number has 12 digits.")
	}
	f, err := s.LoadFamily(ctx, tenantID, guardianID)
	if err != nil {
		return "", false, err
	}
	var b *FamilyBatch
	if f != nil {
		for i := range f.Batches {
			if f.Batches[i].EnrolmentID == enrolmentID {
				b = &f.Batches[i]
			}
		}
	}
	if b == nil {
		return "", false, httpx.Err(httpx.NotFound, "That fee is not on this page.")
	}
	if b.PendingClaim != nil {
		return "", true, nil
	}
	sc := store.For(s.M.DB, tenantID)
	n, err := sc.Count(ctx, "payment_claims", bson.M{"guardian_id": guardianID, "claimed_at": bson.M{"$gt": time.Now().Add(-24 * time.Hour)}})
	if err != nil {
		return "", false, err
	}
	if n >= 10 {
		return "", false, httpx.Err(httpx.OTPRateLimited, "Too many payment notes today. Your teacher will check the ones sent.")
	}
	id := ids.UUIDv7()
	now := time.Now()
	var utrV, appV any
	if utr != "" {
		utrV = utr
	}
	if apps[app] {
		appV = app
	}
	err = s.M.Tx(ctx, func(ctx context.Context) error {
		if err := sc.InsertOne(ctx, "payment_claims", bson.M{
			"_id": id, "enrolment_id": b.EnrolmentID, "guardian_id": guardianID, "amount": b.Due,
			"utr": utrV, "app": appV, "screenshot_url": nil, "claimed_at": now, "status": "pending",
			"created_at": now, "updated_at": now,
		}); err != nil {
			return err
		}
		_, err := changelog.Log(ctx, s.M.DB, tenantID, changelog.Entry{Table: "payment_claims", ID: id, Op: "upsert"})
		return err
	})
	if err != nil {
		return "", false, err
	}
	return id, false, nil
}

// SetConsent records the guardian's choice with the notice version, and puts
// back any message that was waiting for it.
func (s *Service) SetConsent(ctx context.Context, tenantID, guardianID, choice, channel, ip string) error {
	now := time.Now()
	sc := store.For(s.M.DB, tenantID)
	err := s.M.Tx(ctx, func(ctx context.Context) error {
		g, err := sc.FindOne(ctx, "guardians", bson.M{"_id": guardianID, "deleted": bson.M{"$ne": true}})
		if err != nil {
			return err
		}
		if g == nil {
			return httpx.Err(httpx.NotFound, "This page is no longer active.")
		}
		if _, err := sc.UpdateOne(ctx, "guardians", bson.M{"_id": guardianID}, bson.M{"$set": bson.M{"consent": choice, "consent_at": now, "updated_at": now}}, false); err != nil {
			return err
		}
		var ipV any
		if ip != "" {
			ipV = ip
		}
		if err := sc.InsertOne(ctx, "consents", bson.M{
			"_id": ids.UUIDv7(), "guardian_id": guardianID, "choice": choice, "notice_version": ConsentNoticeVersion,
			"phone_hash": g["phone_hash"], "channel": channel, "ip": ipV, "at": now,
		}); err != nil {
			return err
		}
		_, err = changelog.Log(ctx, s.M.DB, tenantID, changelog.Entry{Table: "guardians", ID: guardianID, Op: "upsert"})
		return err
	})
	if err != nil {
		return err
	}
	if choice == "given" {
		_, err = jobs.Release(ctx, s.M.DB, bson.M{"tenant_id": tenantID, "guardian_id": guardianID, "hold_reason": "consent"})
	}
	return err
}

// CreateDataRequest opens a see, correct or erase task for the teacher.
func (s *Service) CreateDataRequest(ctx context.Context, tenantID, guardianID, kind, note string) error {
	sc := store.For(s.M.DB, tenantID)
	n, err := sc.Count(ctx, "data_requests", bson.M{"guardian_id": guardianID, "kind": kind, "status": "open"})
	if err != nil || n > 0 {
		return err
	}
	if r := []rune(note); len(r) > 500 {
		note = string(r[:500])
	}
	return sc.InsertOne(ctx, "data_requests", bson.M{"_id": ids.UUIDv7(), "guardian_id": guardianID, "kind": kind, "note": note, "status": "open", "created_at": time.Now()})
}
