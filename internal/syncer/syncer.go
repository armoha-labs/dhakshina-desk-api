// Package syncer is POST /v1/sync: apply the phone's outbox in one
// transaction, then return everything after the phone's last change number.
// See docs/api-contract.md in the front-end repo.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/changelog"
	"github.com/armohalabs/dhakshina-desk-api/internal/docs"
	"github.com/armohalabs/dhakshina-desk-api/internal/jobs"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/store"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
	"github.com/armohalabs/dhakshina-desk-api/internal/tenancy"
)

var (
	roster = map[string]bool{"tenants": true, "batches": true, "fee_plans": true, "guardians": true, "students": true, "enrolments": true, "payment_claims": true}
	ledger = map[string]bool{"charges": true, "payments": true, "adjustments": true, "messages": true}
	// Writes a read-only class may still make: record money and send receipts.
	readOnlyAllowed = map[string]bool{"payments": true, "payment_claims": true, "messages": true}
)

const (
	MaxPush = 500
	MaxPull = 1000
	// Receipts wait so the teacher's Undo can still cancel them.
	ReceiptHold = 60 * time.Second
)

type Change struct {
	Table string
	Op    string
	ID    string
	Data  map[string]any
	HLC   string
}

type Rejection struct {
	ID      string `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Service struct{ *app.App }

// ParseChanges reads the changes array; malformed entries become rejections.
func ParseChanges(v any) ([]Change, []Rejection) {
	list, _ := v.([]any)
	var out []Change
	var bad []Rejection
	for _, x := range list {
		m := val.M(x)
		id, _ := m["id"].(string)
		table, ok := m["table"].(string)
		if m == nil || !ids.IsUUID(id) || !ok {
			bad = append(bad, Rejection{ID: id, Code: "bad_request", Message: "Each change needs a table and a UUID id."})
			continue
		}
		op, _ := m["op"].(string)
		hlc, _ := m["hlc"].(string)
		out = append(out, Change{Table: table, Op: op, ID: id, Data: val.M(m["data"]), HLC: hlc})
	}
	return out, bad
}

func createdAt(d map[string]any) time.Time {
	if s, ok := d["created_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Now()
}

func withHLC(set bson.M, hlc string) bson.M {
	if hlc != "" {
		set["hlc"] = hlc
	}
	return set
}

func (s *Service) applyOne(ctx context.Context, sc *store.Scoped, c Change, st plans.State) (*changelog.Entry, error) {
	if st.ReadOnly && !readOnlyAllowed[c.Table] {
		return nil, reject("read_only", "Your plan has ended. You can still record payments; renew to make other changes.")
	}
	now := time.Now()

	if c.Table == "tenants" {
		if c.ID != sc.TenantID || c.Op != "upsert" {
			return nil, reject("forbidden", "You can only edit your own class.")
		}
		t, err := sc.Tenant(ctx)
		if err != nil {
			return nil, err
		}
		if old := val.Str(t["hlc"]); c.HLC != "" && old != "" && c.HLC <= old {
			return nil, nil
		}
		fields, err := tenancy.CleanFields(c.Data)
		if err != nil {
			var he *httpx.Error
			if errors.As(err, &he) {
				return nil, reject("bad_request", he.Message)
			}
			return nil, err
		}
		fields["updated_at"] = now
		if _, err := sc.UpdateTenant(ctx, bson.M{"$set": withHLC(fields, c.HLC)}); err != nil {
			return nil, err
		}
		return &changelog.Entry{Table: "tenants", ID: c.ID, Op: "upsert"}, nil
	}

	if roster[c.Table] {
		if c.Op == "insert" {
			c.Op = "upsert"
		}
		if c.Op != "upsert" && c.Op != "delete" {
			return nil, reject("bad_request", "Roster tables take upsert or delete.")
		}
		if c.Op == "delete" && c.Table == "payment_claims" {
			return nil, reject("bad_request", "Claims cannot be deleted.")
		}
		existing, err := sc.FindOne(ctx, c.Table, bson.M{"_id": c.ID})
		if err != nil {
			return nil, err
		}
		// Row-level last writer wins by hybrid logical clock.
		if old := val.Str(existing["hlc"]); existing != nil && old != "" && c.HLC != "" && c.HLC <= old {
			return nil, nil
		}
		if c.Op == "delete" {
			if existing == nil {
				return nil, nil
			}
			if _, err := sc.UpdateOne(ctx, c.Table, bson.M{"_id": c.ID}, bson.M{"$set": withHLC(bson.M{"deleted": true, "updated_at": now}, c.HLC)}, false); err != nil {
				return nil, err
			}
			return &changelog.Entry{Table: c.Table, ID: c.ID, Op: "delete"}, nil
		}
		fields, err := clean(s.Box, c.Table, c.Data, existing)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			if err := s.idFree(ctx, c.Table, c.ID); err != nil {
				return nil, err
			}
			if c.Table == "students" && st.Def.Limits.Students != nil {
				n, err := sc.Count(ctx, "students", bson.M{"deleted": bson.M{"$ne": true}})
				if err != nil {
					return nil, err
				}
				if n >= *st.Def.Limits.Students {
					return nil, reject("plan_limit", fmt.Sprintf("Your plan allows %d students.", *st.Def.Limits.Students))
				}
			}
			if c.Table == "batches" && st.Def.Limits.Batches != nil {
				n, err := sc.Count(ctx, "batches", bson.M{"deleted": bson.M{"$ne": true}, "archived": bson.M{"$ne": true}})
				if err != nil {
					return nil, err
				}
				if n >= *st.Def.Limits.Batches {
					return nil, reject("plan_limit", fmt.Sprintf("Your plan allows %d batches.", *st.Def.Limits.Batches))
				}
			}
		}
		if c.Table == "enrolments" {
			merged := bson.M{}
			for k, v := range existing {
				merged[k] = v
			}
			for k, v := range fields {
				merged[k] = v
			}
			if val.Str(merged["status"]) != "left" {
				clash, err := sc.FindOne(ctx, "enrolments", bson.M{
					"_id": bson.M{"$ne": c.ID}, "student_id": merged["student_id"], "batch_id": merged["batch_id"],
					"status": bson.M{"$in": bson.A{"active", "paused"}}, "deleted": bson.M{"$ne": true},
				})
				if err != nil {
					return nil, err
				}
				if clash != nil {
					return nil, reject("duplicate_enrolment", "This student is already in that batch.")
				}
			}
		}
		fields["updated_at"] = now
		onInsert := bson.M{"created_at": createdAt(c.Data)}
		if c.Table != "payment_claims" {
			onInsert["deleted"] = false
		}
		if _, err := sc.UpdateOne(ctx, c.Table, bson.M{"_id": c.ID}, bson.M{"$set": withHLC(fields, c.HLC), "$setOnInsert": onInsert}, true); err != nil {
			return nil, err
		}
		return &changelog.Entry{Table: c.Table, ID: c.ID, Op: "upsert"}, nil
	}

	// The one edit to a message: Undo cancels it while it is still queued.
	if c.Table == "messages" && c.Op == "upsert" {
		if val.Str(c.Data["status"]) != "cancelled" {
			return nil, reject("bad_request", "A message can only be cancelled.")
		}
		m, err := sc.FindOne(ctx, "messages", bson.M{"_id": c.ID})
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, reject("not_found", "That message is not on the server.")
		}
		switch val.Str(m["status"]) {
		case "cancelled":
			return nil, nil
		case "queued":
		default:
			return nil, reject("already_sent", "This message has already been sent.")
		}
		if _, err := sc.UpdateOne(ctx, "messages", bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"status": "cancelled", "updated_at": now}}, false); err != nil {
			return nil, err
		}
		if _, err := sc.UpdateMany(ctx, "jobs", bson.M{"ref": c.ID, "status": bson.M{"$in": bson.A{jobs.Queued, jobs.Held}}}, bson.M{"$set": bson.M{"status": jobs.Cancelled, "finished_at": now}}); err != nil {
			return nil, err
		}
		return &changelog.Entry{Table: "messages", ID: c.ID, Op: "upsert"}, nil
	}

	if ledger[c.Table] {
		if c.Op != "insert" {
			return nil, reject("bad_request", "Ledger entries are never edited. Reverse with a new entry.")
		}
		if dup, err := sc.FindOne(ctx, c.Table, bson.M{"_id": c.ID}); err != nil || dup != nil {
			return nil, err // retry of an entry already applied
		}
		fields, err := clean(s.Box, c.Table, c.Data, nil)
		if err != nil {
			return nil, err
		}
		if err := s.idFree(ctx, c.Table, c.ID); err != nil {
			return nil, err
		}
		switch c.Table {
		case "charges":
			if fields["kind"] != "item" {
				dup, err := sc.FindOne(ctx, "charges", bson.M{"enrolment_id": fields["enrolment_id"], "cycle": fields["cycle"], "kind": fields["kind"]})
				if err != nil {
					return nil, err
				}
				if dup != nil {
					return nil, reject("duplicate_charge", "This cycle is already charged.")
				}
			}
		case "payments":
			if fields["utr"] != nil {
				dup, err := sc.FindOne(ctx, "payments", bson.M{"utr": fields["utr"]})
				if err != nil {
					return nil, err
				}
				if dup != nil {
					return nil, reject("duplicate_utr", "A payment with this UPI reference is already recorded.")
				}
			}
			t, err := sc.UpdateTenant(ctx, bson.M{"$inc": bson.M{"receipt_seq": 1}})
			if err != nil {
				return nil, err
			}
			prefix := val.Str(t["receipt_prefix"])
			if prefix == "" {
				prefix = "DD"
			}
			fields["receipt_no"] = fmt.Sprintf("%s-%d-%06d", prefix, now.In(val.IST).Year(), val.IntOr(t["receipt_seq"], 0))
		}
		fields["_id"] = c.ID
		fields["created_at"] = createdAt(c.Data)
		fields["updated_at"] = now
		if err := sc.InsertOne(ctx, c.Table, fields); err != nil {
			return nil, err
		}
		if c.Table == "messages" {
			var hold time.Duration
			if fields["kind"] == "receipt" {
				hold = ReceiptHold
			}
			if err := sc.InsertOne(ctx, "jobs", jobs.Doc(jobs.Job{Kind: jobs.SendMessage, Ref: c.ID, RunAfter: now.Add(hold)})); err != nil {
				return nil, err
			}
		}
		return &changelog.Entry{Table: c.Table, ID: c.ID, Op: "insert"}, nil
	}

	return nil, reject("bad_request", "Unknown table "+c.Table+".")
}

// idFree rejects an id another class already uses. Ids are client-made
// UUIDs, so this only happens by mistake or on purpose; without the check the
// insert would hit the unique _id index and abort the whole batch.
func (s *Service) idFree(ctx context.Context, table, id string) error {
	n, err := s.M.C(table).CountDocuments(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if n > 0 {
		return reject("forbidden", "That record belongs to another class.")
	}
	return nil
}

// Push applies changes in one transaction. Used by /v1/sync for the phone
// and the website alike, so every edit reaches other phones through one log.
func (s *Service) Push(ctx context.Context, tenantID string, changes []Change) (accepted []string, rejected []Rejection, err error) {
	if len(changes) > MaxPush {
		return nil, nil, reject("bad_request", fmt.Sprintf("Send at most %d changes per call.", MaxPush))
	}
	accepted, rejected = []string{}, []Rejection{}
	if len(changes) == 0 {
		return
	}
	err = s.M.Tx(ctx, func(ctx context.Context) error {
		accepted, rejected = accepted[:0], rejected[:0]
		sc := store.For(s.M.DB, tenantID)
		t, err := sc.Tenant(ctx)
		if err != nil {
			return err
		}
		if t == nil {
			return reject("not_found", "Class not found.")
		}
		st := plans.StateOf(t, time.Now())
		var log []changelog.Entry
		for _, c := range changes {
			entry, err := s.applyOne(ctx, sc, c, st)
			var rj *Reject
			if errors.As(err, &rj) {
				rejected = append(rejected, Rejection{ID: c.ID, Code: rj.Code, Message: rj.Message})
				continue
			}
			if err != nil {
				return err
			}
			if entry != nil {
				log = append(log, *entry)
			}
			accepted = append(accepted, c.ID)
		}
		_, err = changelog.Log(ctx, s.M.DB, tenantID, log...)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	// Counts kept on the tenant so the admin console never reads class data.
	for _, c := range changes {
		if c.Table == "students" || c.Table == "batches" {
			if err := RefreshStats(ctx, s.M.DB, tenantID); err != nil {
				return accepted, rejected, err
			}
			break
		}
	}
	return accepted, rejected, nil
}

// Sync is the whole /v1/sync round trip.
func (s *Service) Sync(ctx context.Context, tenantID string, body map[string]any) (bson.M, error) {
	since, ok := val.Int(body["since_seq"])
	if !ok || since < 0 {
		since = 0
	}
	if list, _ := body["changes"].([]any); len(list) > MaxPush {
		return nil, reject("bad_request", fmt.Sprintf("Send at most %d changes per call.", MaxPush))
	}
	changes, bad := ParseChanges(body["changes"])
	accepted, rejected, err := s.Push(ctx, tenantID, changes)
	if err != nil {
		return nil, err
	}
	rejected = append(rejected, bad...)
	out, err := s.Pull(ctx, tenantID, since)
	if err != nil {
		return nil, err
	}
	out["accepted"], out["rejected"] = accepted, rejected
	sc := store.For(s.M.DB, tenantID)
	t, err := sc.Tenant(ctx)
	if err != nil {
		return nil, err
	}
	out["entitlements"] = nil
	if t != nil {
		if out["entitlements"], err = plans.Entitlements(ctx, sc, t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Pull returns the changes after since, one page at a time. A document
// changed several times in the page is sent once, at its latest number and
// in its current state.
func (s *Service) Pull(ctx context.Context, tenantID string, since int64) (bson.M, error) {
	sc := store.For(s.M.DB, tenantID)
	rows, err := sc.Find(ctx, "change_log", bson.M{"seq": bson.M{"$gt": since}},
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetLimit(MaxPull+1))
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > MaxPull
	if hasMore {
		rows = rows[:MaxPull]
	}
	seq := since
	if len(rows) > 0 {
		seq = val.IntOr(rows[len(rows)-1]["seq"], since)
	}
	latest := map[string]bson.M{}
	byTable := map[string][]string{}
	for _, r := range rows {
		key := val.Str(r["table"]) + ":" + val.Str(r["doc_id"])
		if _, seen := latest[key]; !seen {
			byTable[val.Str(r["table"])] = append(byTable[val.Str(r["table"])], val.Str(r["doc_id"]))
		}
		latest[key] = r
	}
	found := map[string]bson.M{}
	for table, idList := range byTable {
		var list []bson.M
		if table == "tenants" {
			t, err := sc.Tenant(ctx)
			if err != nil {
				return nil, err
			}
			if t != nil {
				list = []bson.M{t}
			}
		} else if list, err = sc.Find(ctx, table, bson.M{"_id": bson.M{"$in": idList}}); err != nil {
			return nil, err
		}
		for _, d := range list {
			found[table+":"+val.Str(d["_id"])] = d
		}
	}
	entries := make([]bson.M, 0, len(latest))
	for _, r := range latest {
		entries = append(entries, r)
	}
	sort.Slice(entries, func(i, j int) bool { return val.IntOr(entries[i]["seq"], 0) < val.IntOr(entries[j]["seq"], 0) })
	changes := make([]bson.M, 0, len(entries))
	for _, r := range entries {
		table, id := val.Str(r["table"]), val.Str(r["doc_id"])
		d := found[table+":"+id]
		if d == nil {
			continue
		}
		op := "upsert"
		if val.Bool(d["deleted"]) {
			op = "delete"
		} else if val.Str(r["op"]) == "insert" {
			op = "insert"
		}
		changes = append(changes, bson.M{"seq": r["seq"], "table": table, "op": op, "id": id, "data": docs.ToAPI(s.Box, table, d)})
	}
	return bson.M{"seq": seq, "has_more": hasMore, "changes": changes}, nil
}

// RefreshStats stores the class's student and batch counts on the tenant.
func RefreshStats(ctx context.Context, db *mongo.Database, tenantID string) error {
	sc := store.For(db, tenantID)
	students, err := sc.Count(ctx, "students", bson.M{"deleted": bson.M{"$ne": true}})
	if err != nil {
		return err
	}
	batches, err := sc.Count(ctx, "batches", bson.M{"deleted": bson.M{"$ne": true}, "archived": bson.M{"$ne": true}})
	if err != nil {
		return err
	}
	_, err = db.Collection("tenants").UpdateOne(ctx, bson.M{"_id": tenantID}, bson.M{"$set": bson.M{"stats": bson.M{"students": students, "batches": batches, "at": time.Now()}}})
	return err
}
