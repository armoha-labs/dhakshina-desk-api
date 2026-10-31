package messaging

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/changelog"
	"github.com/armohalabs/dhakshina-desk-api/internal/jobs"
	"github.com/armohalabs/dhakshina-desk-api/internal/plans"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/store"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
	"github.com/armohalabs/dhakshina-desk-api/internal/syncer"
)

// Reminder stages, in days after the due date. One reminder per enrolment,
// cycle and stage (unique index on reminders), so reruns never double-send.
var stages = []struct {
	Name string
	Days int
}{{"due", 0}, {"overdue_3", 3}, {"overdue_10", 10}}

const dailyHour = 7 // IST; sends are spread from 9 am

// Schedule runs the daily pass once per IST day, from 7 am, on whichever
// instance gets there first.
func (w *Worker) Schedule(ctx context.Context) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		if err := w.DailyPass(ctx, w.Now()); err != nil {
			slog.Error("daily_pass_failed", "err", err)
		}
		if err := w.QueueOpenErasures(ctx); err != nil {
			slog.Error("queue_erasures_failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// DailyPass queues one daily_tenant job per active class.
func (w *Worker) DailyPass(ctx context.Context, now time.Time) error {
	l := now.In(val.IST)
	if l.Hour() < dailyHour {
		return nil
	}
	date := l.Format("2006-01-02")
	if _, err := w.M.C("scheduler_runs").InsertOne(ctx, bson.M{"_id": "daily:" + date, "at": now}); err != nil {
		if db.IsDup(err) {
			return nil
		}
		return err
	}
	// Messages held for this month's quota get another chance.
	if _, err := jobs.Release(ctx, w.M.DB, bson.M{"hold_reason": "quota"}); err != nil {
		return err
	}
	cur, err := w.M.C("tenants").Find(ctx, bson.M{"status": bson.M{"$ne": "suspended"}}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return err
	}
	var ts []bson.M
	if err := cur.All(ctx, &ts); err != nil {
		return err
	}
	for _, t := range ts {
		if err := jobs.Enqueue(ctx, w.M.DB, jobs.Job{Kind: jobs.DailyTenant, TenantID: val.Str(t["_id"]), Payload: bson.M{"date": date}}); err != nil {
			return err
		}
	}
	slog.Info("daily_pass_queued", "date", date, "classes", len(ts))
	return nil
}

func (w *Worker) dailyTenant(ctx context.Context, j bson.M) error {
	tid := val.Str(j["tenant_id"])
	date := val.Str(val.M(j["payload"])["date"])
	sc := store.For(w.M.DB, tid)
	t, err := sc.Tenant(ctx)
	if err != nil {
		return err
	}
	if t == nil {
		return jobs.Finish(ctx, w.M.DB, j["_id"], "tenant gone")
	}
	if err := syncer.RefreshStats(ctx, w.M.DB, tid); err != nil {
		return err
	}
	if err := w.trialNotice(ctx, t, date); err != nil {
		return err
	}
	n, err := w.queueReminders(ctx, t, date)
	if err != nil {
		return err
	}
	return jobs.Finish(ctx, w.M.DB, j["_id"], "reminders:"+strconv.Itoa(n))
}

// trialNotice sends the owner a signed upgrade link 3 days and 1 day before
// the trial ends, once each.
func (w *Worker) trialNotice(ctx context.Context, t bson.M, date string) error {
	if val.Str(t["plan"]) != "trial" || val.Bool(t["founder"]) {
		return nil
	}
	ends, ok := val.Date(t["trial_ends_on"])
	if !ok {
		return nil
	}
	today, _ := time.ParseInLocation("2006-01-02", date, val.IST)
	end, _ := time.ParseInLocation("2006-01-02", ends, val.IST)
	left := int(end.Sub(today).Hours() / 24)
	if left != 3 && left != 1 {
		return nil
	}
	key := "trial_notice_" + strconv.Itoa(left)
	tid := val.Str(t["_id"])
	res, err := w.M.C("tenants").UpdateOne(ctx, bson.M{"_id": tid, "notices." + key: bson.M{"$exists": false}}, bson.M{"$set": bson.M{"notices." + key: time.Now()}})
	if err != nil || res.ModifiedCount == 0 {
		return err
	}
	var owner bson.M
	if err := w.M.C("memberships").FindOne(ctx, bson.M{"tenant_id": tid, "role": "owner", "removed": bson.M{"$ne": true}}).Decode(&owner); err != nil {
		return nil
	}
	tok, _ := w.Links.Upgrade(tid, val.Str(owner["user_id"]))
	return jobs.Enqueue(ctx, w.M.DB, jobs.Job{Kind: jobs.NotifyOwner, TenantID: tid, Ref: key, Payload: bson.M{
		"user_id": owner["user_id"], "template": TplTrialEnding, "params": bson.A{val.Str(t["name"]), ends}, "button": tok,
	}})
}

// queueReminders creates the day's automatic reminders for a class on a plan
// with reminders, unless the class turned them off (tenants.auto_reminders:
// false). Each reminder is a messages row the phone sees through sync, plus
// a send job spread over 9 to 11 am.
func (w *Worker) queueReminders(ctx context.Context, t bson.M, date string) (int, error) {
	st := plans.StateOf(t, w.Now())
	if st.ReadOnly || !st.Features["reminders"] || st.Def.Limits.WhatsAppPerMonth == 0 {
		return 0, nil
	}
	if v, ok := t["auto_reminders"].(bool); ok && !v {
		return 0, nil
	}
	tid := val.Str(t["_id"])
	sc := store.For(w.M.DB, tid)
	live := bson.M{"$ne": true}
	enrolments, err := sc.Find(ctx, "enrolments", bson.M{"status": "active", "deleted": live})
	if err != nil {
		return 0, err
	}
	if len(enrolments) == 0 {
		return 0, nil
	}
	eids := make([]string, len(enrolments))
	for i, e := range enrolments {
		eids[i] = val.Str(e["_id"])
	}
	in := bson.M{"enrolment_id": bson.M{"$in": eids}}
	charges, err := sc.Find(ctx, "charges", in)
	if err != nil {
		return 0, err
	}
	totals := func(table string) (map[string]int64, error) {
		rows, err := sc.Aggregate(ctx, table, bson.A{bson.M{"$match": in}, bson.M{"$group": bson.M{"_id": "$enrolment_id", "n": bson.M{"$sum": "$amount"}}}})
		out := map[string]int64{}
		for _, r := range rows {
			out[val.Str(r["_id"])] = val.IntOr(r["n"], 0)
		}
		return out, err
	}
	paid, err := totals("payments")
	if err != nil {
		return 0, err
	}
	adj, err := totals("adjustments")
	if err != nil {
		return 0, err
	}
	byE := map[string][]bson.M{}
	for _, c := range charges {
		byE[val.Str(c["enrolment_id"])] = append(byE[val.Str(c["enrolment_id"])], c)
	}
	today, _ := time.ParseInLocation("2006-01-02", date, val.IST)
	queued := 0
	for _, e := range enrolments {
		eid := val.Str(e["_id"])
		cs := byE[eid]
		sortByDue(cs)
		// Oldest open charge, settling payments and credits oldest first.
		credit := paid[eid] + adj[eid]
		var open bson.M
		for _, c := range cs {
			amt := val.IntOr(c["amount"], 0)
			if credit >= amt {
				credit -= amt
				continue
			}
			open = c
			break
		}
		if open == nil {
			continue
		}
		dueOn, err := time.ParseInLocation("2006-01-02", val.Str(open["due_on"]), val.IST)
		if err != nil {
			continue
		}
		age := int(today.Sub(dueOn).Hours() / 24)
		stage := ""
		for _, s := range stages {
			// Only on the day a stage is reached (or the day after, if a run
			// was missed), so a long-unpaid fee is not reminded every day.
			if age == s.Days || age == s.Days+1 {
				stage = s.Name
			}
		}
		if stage == "" {
			continue
		}
		st, err := sc.FindOne(ctx, "students", bson.M{"_id": e["student_id"], "deleted": live})
		if err != nil || st == nil {
			continue
		}
		gid := val.Str(st["guardian_id"])
		ok, err := w.queueReminder(ctx, sc, eid, gid, val.Str(open["cycle"]), stage, today)
		if err != nil {
			return queued, err
		}
		if ok {
			queued++
		}
	}
	return queued, nil
}

func (w *Worker) queueReminder(ctx context.Context, sc *store.Scoped, eid, gid, cycle, stage string, today time.Time) (bool, error) {
	msgID := ids.UUIDv7()
	now := time.Now()
	window := time.Date(today.Year(), today.Month(), today.Day(), 9, 0, 0, 0, val.IST).Add(time.Duration(rand.IntN(7200)) * time.Second)
	if window.Before(now) {
		window = now.Add(time.Duration(rand.IntN(600)) * time.Second)
	}
	err := w.M.Tx(ctx, func(ctx context.Context) error {
		if err := sc.InsertOne(ctx, "reminders", bson.M{"_id": ids.UUIDv7(), "enrolment_id": eid, "cycle": cycle, "stage": stage, "message_id": msgID, "created_at": now}); err != nil {
			return err
		}
		if err := sc.InsertOne(ctx, "messages", bson.M{
			"_id": msgID, "kind": "reminder", "guardian_id": gid, "enrolment_id": eid, "payment_id": nil,
			"status": "queued", "origin": "scheduler", "stage": stage, "created_at": now, "updated_at": now,
		}); err != nil {
			return err
		}
		if err := sc.InsertOne(ctx, "jobs", jobs.Doc(jobs.Job{Kind: jobs.SendMessage, Ref: msgID, RunAfter: window})); err != nil {
			return err
		}
		_, err := changelog.Log(ctx, w.M.DB, sc.TenantID, changelog.Entry{Table: "messages", ID: msgID, Op: "insert"})
		return err
	})
	if db.IsDup(err) {
		return false, nil
	}
	return err == nil, err
}

func sortByDue(cs []bson.M) {
	sort.SliceStable(cs, func(i, j int) bool { return val.Str(cs[i]["due_on"]) < val.Str(cs[j]["due_on"]) })
}
