// Package indexes creates every index. Class-data indexes start with
// tenant_id; unique indexes back the no-duplicate rules (UTR per class, one
// charge per cycle, one reminder per stage). A few system lookups (message by
// WhatsApp id, guardian by phone across classes) are deliberately global.
package indexes

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type ix struct {
	coll string
	keys bson.D
	opts *options.IndexOptionsBuilder
}

func k(pairs ...any) bson.D {
	d := bson.D{}
	for i := 0; i < len(pairs); i += 2 {
		d = append(d, bson.E{Key: pairs[i].(string), Value: pairs[i+1]})
	}
	return d
}

func Ensure(ctx context.Context, db *mongo.Database) error {
	u := func() *options.IndexOptionsBuilder { return options.Index().SetUnique(true) }
	ttl := func(s int32) *options.IndexOptionsBuilder { return options.Index().SetExpireAfterSeconds(s) }
	list := []ix{
		{"users", k("phone_hash", 1), u()},
		{"memberships", k("user_id", 1, "tenant_id", 1), u()},
		{"memberships", k("tenant_id", 1), nil},
		{"otp_requests", k("phone_hash", 1, "created_at", -1), nil},
		{"otp_requests", k("expires_at", 1), ttl(3600)},
		{"refresh_tokens", k("user_id", 1), nil},
		{"refresh_tokens", k("expires_at", 1), ttl(0)},
		{"change_log", k("tenant_id", 1, "seq", 1), u()},
		{"batches", k("tenant_id", 1, "archived", 1), nil},
		{"fee_plans", k("tenant_id", 1), nil},
		{"guardians", k("tenant_id", 1, "phone_hash", 1), nil},
		{"guardians", k("phone_hash", 1), nil},
		{"students", k("tenant_id", 1, "guardian_id", 1), nil},
		{"enrolments", k("tenant_id", 1, "student_id", 1, "batch_id", 1), nil},
		{"enrolments", k("tenant_id", 1, "status", 1), nil},
		{"charges", k("tenant_id", 1, "enrolment_id", 1, "cycle", 1, "kind", 1), u().SetPartialFilterExpression(bson.M{"dedupe": true})},
		{"charges", k("tenant_id", 1, "enrolment_id", 1, "due_on", 1), nil},
		{"payments", k("tenant_id", 1, "enrolment_id", 1, "paid_on", -1), nil},
		{"payments", k("tenant_id", 1, "utr", 1), u().SetPartialFilterExpression(bson.M{"utr": bson.M{"$type": "string"}})},
		{"adjustments", k("tenant_id", 1, "enrolment_id", 1), nil},
		{"payment_claims", k("tenant_id", 1, "enrolment_id", 1, "status", 1), nil},
		{"payment_claims", k("tenant_id", 1, "guardian_id", 1, "claimed_at", -1), nil},
		{"messages", k("tenant_id", 1, "status", 1), nil},
		{"messages", k("wa_id", 1), options.Index().SetPartialFilterExpression(bson.M{"wa_id": bson.M{"$type": "string"}})},
		{"reminders", k("tenant_id", 1, "enrolment_id", 1, "cycle", 1, "stage", 1), u()},
		{"jobs", k("status", 1, "run_after", 1), nil},
		{"jobs", k("status", 1, "lease_until", 1), nil},
		{"jobs", k("tenant_id", 1, "ref", 1), nil},
		{"jobs", k("tenant_id", 1, "status", 1, "hold_reason", 1), nil},
		{"jobs", k("finished_at", 1), ttl(30 * 86400)},
		{"wa_events", k("received_at", 1), ttl(30 * 86400)},
		{"inbound_messages", k("wa_id", 1), u()},
		{"inbound_messages", k("from_hash", 1, "at", -1), nil},
		{"consents", k("tenant_id", 1, "guardian_id", 1, "at", -1), nil},
		{"data_requests", k("tenant_id", 1, "status", 1), nil},
		{"usage_counters", k("tenant_id", 1, "month", 1), u()},
		{"invoices", k("tenant_id", 1, "created_at", -1), nil},
		{"invoices", k("status", 1, "paid_at", 1), nil},
		{"audit_log", k("at", -1), nil},
		{"audit_log", k("tenant_id", 1, "at", -1), nil},
		{"tenants", k("plan", 1, "trial_ends_on", 1), nil},
		{"tenants", k("founder", 1), nil},
	}
	for _, i := range list {
		m := mongo.IndexModel{Keys: i.keys}
		if i.opts != nil {
			m.Options = i.opts
		}
		if _, err := db.Collection(i.coll).Indexes().CreateOne(ctx, m); err != nil {
			return err
		}
	}
	return nil
}
