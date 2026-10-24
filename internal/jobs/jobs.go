// Package jobs is the queue: jobs are documents in the jobs collection,
// written in the same MongoDB transaction as the change that causes them, so
// a receipt is never lost and never sent for a payment that rolled back.
// Workers claim a job with one atomic findOneAndUpdate and a lease; a job
// whose worker dies is picked up again when the lease runs out.
package jobs

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
)

// Statuses. A held job waits for something outside it (plan, consent, quota)
// and is put back with Release.
const (
	Queued    = "queued"
	Running   = "running"
	Done      = "done"
	Failed    = "failed"
	Cancelled = "cancelled"
	Held      = "held"
)

// Kinds.
const (
	SendMessage = "send_message" // ref: messages._id
	WAEvent     = "wa_event"     // ref: wa_events._id
	DailyTenant = "daily_tenant" // tenant_id; payload.date
	NotifyOwner = "notify_owner" // payload: template, params, user_id, button
)

const Lease = 2 * time.Minute

type Job struct {
	Kind     string
	TenantID string
	Ref      string
	RunAfter time.Time
	Payload  bson.M
}

// Doc builds the document to insert. Use it inside the caller's transaction.
func Doc(j Job) bson.M {
	now := time.Now()
	if j.RunAfter.IsZero() {
		j.RunAfter = now
	}
	d := bson.M{"_id": ids.UUIDv7(), "kind": j.Kind, "ref": j.Ref, "status": Queued, "run_after": j.RunAfter, "attempts": 0, "created_at": now}
	if j.TenantID != "" {
		d["tenant_id"] = j.TenantID
	}
	if j.Payload != nil {
		d["payload"] = j.Payload
	}
	return d
}

func Enqueue(ctx context.Context, db *mongo.Database, j Job) error {
	_, err := db.Collection("jobs").InsertOne(ctx, Doc(j))
	return err
}

// Claim takes the next due job, or returns nil when there is none.
func Claim(ctx context.Context, db *mongo.Database, worker string) (bson.M, error) {
	now := time.Now()
	var j bson.M
	err := db.Collection("jobs").FindOneAndUpdate(ctx,
		bson.M{"$or": bson.A{
			bson.M{"status": Queued, "run_after": bson.M{"$lte": now}},
			bson.M{"status": Running, "lease_until": bson.M{"$lt": now}},
		}},
		bson.M{"$set": bson.M{"status": Running, "lease_until": now.Add(Lease), "worker": worker, "started_at": now}, "$inc": bson.M{"attempts": 1}},
		options.FindOneAndUpdate().SetSort(bson.D{{Key: "run_after", Value: 1}}).SetReturnDocument(options.After),
	).Decode(&j)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return j, err
}

func set(ctx context.Context, db *mongo.Database, id any, fields bson.M) error {
	_, err := db.Collection("jobs").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields, "$unset": bson.M{"lease_until": ""}})
	return err
}

func Finish(ctx context.Context, db *mongo.Database, id any, note string) error {
	return set(ctx, db, id, bson.M{"status": Done, "finished_at": time.Now(), "note": note})
}

func Fail(ctx context.Context, db *mongo.Database, id any, reason string) error {
	return set(ctx, db, id, bson.M{"status": Failed, "finished_at": time.Now(), "last_error": reason})
}

// Retry puts the job back to run after a delay.
func Retry(ctx context.Context, db *mongo.Database, id any, after time.Duration, reason string) error {
	return set(ctx, db, id, bson.M{"status": Queued, "run_after": time.Now().Add(after), "last_error": reason})
}

// Later puts the job back without counting the run as an attempt (quiet hours).
func Later(ctx context.Context, db *mongo.Database, id any, at time.Time, reason string) error {
	_, err := db.Collection("jobs").UpdateOne(ctx, bson.M{"_id": id}, bson.M{
		"$set": bson.M{"status": Queued, "run_after": at, "note": reason}, "$inc": bson.M{"attempts": -1}, "$unset": bson.M{"lease_until": ""},
	})
	return err
}

// Hold parks the job until Release; extra fields say what it waits for.
func Hold(ctx context.Context, db *mongo.Database, id any, reason string, extra bson.M) error {
	f := bson.M{"status": Held, "hold_reason": reason, "held_at": time.Now()}
	for k, v := range extra {
		f[k] = v
	}
	_, err := db.Collection("jobs").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": f, "$inc": bson.M{"attempts": -1}, "$unset": bson.M{"lease_until": ""}})
	return err
}

// Release puts held jobs matching filter back in the queue.
func Release(ctx context.Context, db *mongo.Database, filter bson.M) (int64, error) {
	f := bson.M{"status": Held}
	for k, v := range filter {
		f[k] = v
	}
	res, err := db.Collection("jobs").UpdateMany(ctx, f, bson.M{"$set": bson.M{"status": Queued, "run_after": time.Now()}, "$unset": bson.M{"hold_reason": ""}})
	if err != nil {
		return 0, err
	}
	return res.ModifiedCount, nil
}
