package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/changelog"
	"github.com/armohalabs/dhakshina-desk-api/internal/collections"
	"github.com/armohalabs/dhakshina-desk-api/internal/jobs"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/store"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/whatsapp"
)

const maxAttempts = 6

type Worker struct {
	*app.App
	Family *collections.Service
	name   string
	// Now is overridable in tests.
	Now func() time.Time
}

func NewWorker(a *app.App, family *collections.Service) *Worker {
	host, _ := os.Hostname()
	return &Worker{App: a, Family: family, name: fmt.Sprintf("%s-%d", host, os.Getpid()), Now: time.Now}
}

// Run claims and runs jobs until ctx ends. A panic in one job fails that job,
// never the process.
func (w *Worker) Run(ctx context.Context) {
	slog.Info("worker_started", "name", w.name)
	idle := time.NewTimer(0)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
		}
		for ctx.Err() == nil {
			ran, err := w.RunOne(ctx)
			if err != nil {
				slog.Error("worker_claim_failed", "err", err)
				break
			}
			if !ran {
				break
			}
		}
		idle.Reset(2 * time.Second)
	}
}

// RunOne runs the next due job; false when there was none.
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	j, err := jobs.Claim(ctx, w.M.DB, w.name)
	if err != nil || j == nil {
		return false, err
	}
	w.handle(ctx, j)
	return true, nil
}

func (w *Worker) handle(ctx context.Context, j bson.M) {
	id := j["_id"]
	kind := val.Str(j["kind"])
	defer func() {
		if v := recover(); v != nil {
			slog.Error("job_panic", "kind", kind, "value", v)
			_ = jobs.Fail(ctx, w.M.DB, id, fmt.Sprint("panic: ", v))
		}
	}()
	jctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var err error
	switch kind {
	case jobs.SendMessage:
		err = w.sendMessage(jctx, j)
	case jobs.NotifyOwner:
		err = w.notifyOwner(jctx, j)
	case jobs.WAEvent:
		err = w.processEvent(jctx, j)
	case jobs.DailyTenant:
		err = w.dailyTenant(jctx, j)
	default:
		err = jobs.Fail(jctx, w.M.DB, id, "unknown job kind")
	}
	if err != nil {
		attempts := val.IntOr(j["attempts"], 1)
		slog.Error("job_failed", "kind", kind, "attempt", attempts, "err", err)
		if attempts >= maxAttempts {
			_ = jobs.Fail(ctx, w.M.DB, id, err.Error())
			return
		}
		_ = jobs.Retry(ctx, w.M.DB, id, time.Duration(attempts*attempts)*time.Minute, err.Error())
	}
}

// QuietUntil returns when sending may resume, or zero when it may send now.
// No sends between 9 pm and 8 am IST.
func QuietUntil(now time.Time) time.Time {
	l := now.In(val.IST)
	if l.Hour() >= 8 && l.Hour() < 21 {
		return time.Time{}
	}
	next := time.Date(l.Year(), l.Month(), l.Day(), 8, 0, 0, 0, val.IST)
	if l.Hour() >= 21 {
		next = next.AddDate(0, 0, 1)
	}
	// Spread the morning's backlog over the first half hour.
	return next.Add(time.Duration(rand.IntN(1800)) * time.Second)
}

// setStatus updates a message and logs it so the phone sees the new status.
func (w *Worker) setStatus(ctx context.Context, tenantID, msgID string, set bson.M) error {
	set["updated_at"] = time.Now()
	return w.M.Tx(ctx, func(ctx context.Context) error {
		if _, err := store.For(w.M.DB, tenantID).UpdateOne(ctx, "messages", bson.M{"_id": msgID}, bson.M{"$set": set}, false); err != nil {
			return err
		}
		_, err := changelog.Log(ctx, w.M.DB, tenantID, changelog.Entry{Table: "messages", ID: msgID, Op: "upsert"})
		return err
	})
}

func (w *Worker) sendMessage(ctx context.Context, j bson.M) error {
	id, tid, ref := j["_id"], val.Str(j["tenant_id"]), val.Str(j["ref"])
	sc := store.For(w.M.DB, tid)
	m, err := sc.FindOne(ctx, "messages", bson.M{"_id": ref})
	if err != nil {
		return err
	}
	if m == nil || val.Str(m["status"]) != "queued" {
		return jobs.Finish(ctx, w.M.DB, id, "not queued")
	}
	t, err := sc.Tenant(ctx)
	if err != nil {
		return err
	}
	if t == nil {
		return jobs.Fail(ctx, w.M.DB, id, "tenant missing")
	}
	kind := val.Str(m["kind"])
	st := plans.StateOf(t, w.Now())
	// The server only sends when the plan allows it; otherwise the message
	// stays queued until the plan changes.
	if val.Str(t["status"]) == "suspended" || st.Def.Limits.WhatsAppPerMonth == 0 || (kind != "receipt" && !st.Features["reminders"]) {
		return jobs.Hold(ctx, w.M.DB, id, "plan", nil)
	}
	if at := QuietUntil(w.Now()); !at.IsZero() {
		return jobs.Later(ctx, w.M.DB, id, at, "quiet hours")
	}
	gid := val.Str(m["guardian_id"])
	g, err := sc.FindOne(ctx, "guardians", bson.M{"_id": gid, "deleted": bson.M{"$ne": true}})
	if err != nil {
		return err
	}
	phone := w.Box.DecryptOr(g["phone_enc"])
	if g == nil || phone == "" {
		if err := w.setStatus(ctx, tid, ref, bson.M{"status": "failed", "error": "no_guardian"}); err != nil {
			return err
		}
		return jobs.Fail(ctx, w.M.DB, id, "no guardian")
	}
	// DPDP: no reminders until the guardian has agreed. Receipts for a
	// payment they made still go. The first held message asks for consent.
	switch val.Str(g["consent"]) {
	case "withdrawn":
		if err := w.setStatus(ctx, tid, ref, bson.M{"status": "failed", "error": "consent_withdrawn"}); err != nil {
			return err
		}
		return jobs.Fail(ctx, w.M.DB, id, "consent withdrawn")
	case "given":
	default:
		if kind != "receipt" {
			if err := w.askConsent(ctx, t, g, phone); err != nil {
				return err
			}
			return jobs.Hold(ctx, w.M.DB, id, "consent", bson.M{"guardian_id": gid})
		}
	}
	month := plans.MonthKey(w.Now())
	usage, err := sc.FindOne(ctx, "usage_counters", bson.M{"month": month})
	if err != nil {
		return err
	}
	if val.IntOr(usage["whatsapp"], 0) >= st.Def.Limits.WhatsAppPerMonth {
		return jobs.Hold(ctx, w.M.DB, id, "quota", nil)
	}
	tpl, skip, err := w.buildTemplate(ctx, t, m, gid)
	if err != nil {
		return err
	}
	if skip != "" {
		if err := w.setStatus(ctx, tid, ref, bson.M{"status": "cancelled", "error": skip}); err != nil {
			return err
		}
		return jobs.Finish(ctx, w.M.DB, id, skip)
	}
	waID, err := w.WA.SendTemplate(ctx, phone, tpl)
	if err != nil {
		var we *whatsapp.Error
		if errors.As(err, &we) && we.Permanent || val.IntOr(j["attempts"], 1) >= maxAttempts {
			if e2 := w.setStatus(ctx, tid, ref, bson.M{"status": "failed", "error": err.Error()}); e2 != nil {
				return e2
			}
			return jobs.Fail(ctx, w.M.DB, id, err.Error())
		}
		return err
	}
	if _, err := sc.UpdateOne(ctx, "usage_counters", bson.M{"month": month}, bson.M{"$inc": bson.M{"whatsapp": 1}}, true); err != nil {
		slog.Error("usage_count_failed", "err", err)
	}
	if err := w.setStatus(ctx, tid, ref, bson.M{"status": "sent", "wa_id": waID, "sent_at": time.Now(), "template": tpl.Name}); err != nil {
		return err
	}
	return jobs.Finish(ctx, w.M.DB, id, "sent")
}

// buildTemplate fills the template for a message. A non-empty skip reason
// means there is nothing to send (for example the fee was paid meanwhile).
func (w *Worker) buildTemplate(ctx context.Context, t, m bson.M, gid string) (whatsapp.Template, string, error) {
	tid := val.Str(t["_id"])
	class := val.Str(t["name"])
	token, _ := w.Links.Parent(tid, gid)
	sc := store.For(w.M.DB, tid)
	eid := val.Str(m["enrolment_id"])
	student, batch := "", ""
	if eid != "" {
		if e, err := sc.FindOne(ctx, "enrolments", bson.M{"_id": eid}); err != nil {
			return whatsapp.Template{}, "", err
		} else if e != nil {
			st, _ := sc.FindOne(ctx, "students", bson.M{"_id": e["student_id"]})
			b, _ := sc.FindOne(ctx, "batches", bson.M{"_id": e["batch_id"]})
			student, batch = val.Str(st["name"]), val.Str(b["name"])
		}
	}
	switch val.Str(m["kind"]) {
	case "receipt":
		p, err := sc.FindOne(ctx, "payments", bson.M{"_id": m["payment_id"]})
		if err != nil {
			return whatsapp.Template{}, "", err
		}
		if p == nil {
			return whatsapp.Template{}, "payment_missing", nil
		}
		if eid == "" {
			e, _ := sc.FindOne(ctx, "enrolments", bson.M{"_id": p["enrolment_id"]})
			st, _ := sc.FindOne(ctx, "students", bson.M{"_id": e["student_id"]})
			b, _ := sc.FindOne(ctx, "batches", bson.M{"_id": e["batch_id"]})
			student, batch = val.Str(st["name"]), val.Str(b["name"])
		}
		return whatsapp.Template{Name: TplFeeReceipt, ButtonURLSuffix: token, Params: []string{
			class, collections.Rupees(val.IntOr(p["amount"], 0)), orDash(student), orDash(batch), orDash(val.Str(p["receipt_no"])), val.Str(p["paid_on"]),
		}}, "", nil
	case "reminder":
		f, err := w.Family.LoadFamily(ctx, tid, gid)
		if err != nil {
			return whatsapp.Template{}, "", err
		}
		var due int64
		note := ""
		if f != nil {
			for _, b := range f.Batches {
				if eid == "" || b.EnrolmentID == eid {
					due += b.Due
					if note == "" && b.Due > 0 {
						note = strings.TrimPrefix(strings.TrimPrefix(b.Note, "Overdue · "), "Due · ")
					}
				}
			}
		}
		if due <= 0 {
			return whatsapp.Template{}, "nothing_due", nil
		}
		return whatsapp.Template{Name: TplFeeReminder, ButtonURLSuffix: token, Params: []string{
			class, orDash(student), orDash(batch), collections.Rupees(due), orDash(note),
		}}, "", nil
	default: // fee_notice
		return whatsapp.Template{Name: TplFeeNotice, ButtonURLSuffix: token, Params: []string{class, orDash(student), orDash(batch)}}, "", nil
	}
}

// askConsent sends the consent request once per guardian.
func (w *Worker) askConsent(ctx context.Context, t, g bson.M, phone string) error {
	if g["consent_requested_at"] != nil {
		return nil
	}
	tid, gid := val.Str(t["_id"]), val.Str(g["_id"])
	token, _ := w.Links.Parent(tid, gid)
	ref := tid + ":" + gid
	if _, err := w.WA.SendTemplate(ctx, phone, whatsapp.Template{
		Name: TplConsentRequest, ButtonURLSuffix: token + "/consent",
		Params:       []string{val.Str(t["name"]), orDash(val.Str(g["name"]))},
		QuickReplies: []string{"consent:given:" + ref, "consent:withdrawn:" + ref},
	}); err != nil {
		return err
	}
	_, err := store.For(w.M.DB, tid).UpdateOne(ctx, "guardians", bson.M{"_id": gid}, bson.M{"$set": bson.M{"consent_requested_at": time.Now()}}, false)
	return err
}

// notifyOwner sends a platform message to a class owner.
func (w *Worker) notifyOwner(ctx context.Context, j bson.M) error {
	p := val.M(j["payload"])
	var user bson.M
	err := w.M.C("users").FindOne(ctx, bson.M{"_id": p["user_id"]}).Decode(&user)
	phone := w.Box.DecryptOr(user["phone_enc"])
	if err != nil || phone == "" {
		return jobs.Fail(ctx, w.M.DB, j["_id"], "owner has no phone")
	}
	var params []string
	for _, x := range asList(p["params"]) {
		params = append(params, fmt.Sprint(x))
	}
	if _, err := w.WA.SendTemplate(ctx, phone, whatsapp.Template{Name: val.Str(p["template"]), Params: params, ButtonURLSuffix: val.Str(p["button"])}); err != nil {
		var we *whatsapp.Error
		if errors.As(err, &we) && we.Permanent {
			return jobs.Fail(ctx, w.M.DB, j["_id"], err.Error())
		}
		return err
	}
	return jobs.Finish(ctx, w.M.DB, j["_id"], "sent")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func asList(v any) []any {
	switch x := v.(type) {
	case bson.A:
		return x
	case []any:
		return x
	}
	return nil
}
