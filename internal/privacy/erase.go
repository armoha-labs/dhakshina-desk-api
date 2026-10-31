package privacy

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

// AccountErase is the data_requests kind DELETE /v1/me opens.
const AccountErase = "account_erase"

// classData is every class collection with nothing the law asks us to keep.
// Payments, invoices and the audit log are kept on purpose: see eraseClass.
var classData = []string{
	"batches", "fee_plans", "guardians", "students", "enrolments", "charges", "adjustments",
	"payment_claims", "messages", "reminders", "consents", "change_log", "jobs",
	"usage_counters", "subscriptions",
}

// EraseAccount carries out an open account_erase request. It is safe to run
// again after a failure: each step only removes what is left, and the
// memberships that lead back to a class go last.
func EraseAccount(ctx context.Context, d *mongo.Database, requestID string) error {
	var req bson.M
	err := d.Collection("data_requests").FindOne(ctx, bson.M{"_id": requestID, "kind": AccountErase, "status": "open"}).Decode(&req)
	if db.NotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	userID := val.Str(req["user_id"])
	cur, err := d.Collection("memberships").Find(ctx, bson.M{"user_id": userID})
	if err != nil {
		return err
	}
	var ms []bson.M
	if err := cur.All(ctx, &ms); err != nil {
		return err
	}
	for _, m := range ms {
		tid := val.Str(m["tenant_id"])
		if m["removed"] != true && val.Str(m["role"]) == "owner" {
			others, err := d.Collection("memberships").CountDocuments(ctx, bson.M{
				"tenant_id": tid, "role": "owner", "removed": bson.M{"$ne": true}, "user_id": bson.M{"$ne": userID},
			})
			if err != nil {
				return err
			}
			if others == 0 {
				if err := eraseClass(ctx, d, tid, userID); err != nil {
					return err
				}
				continue
			}
		}
		if _, err := d.Collection("memberships").DeleteOne(ctx, bson.M{"_id": m["_id"]}); err != nil {
			return err
		}
	}
	if err := eraseUser(ctx, d, userID); err != nil {
		return err
	}
	if err := Audit(ctx, d, "user:"+userID, "account_erased", val.Str(req["tenant_id"]), nil); err != nil {
		return err
	}
	_, err = d.Collection("data_requests").UpdateOne(ctx, bson.M{"_id": requestID}, bson.M{"$set": bson.M{"status": "done", "done_at": time.Now()}})
	return err
}

// eraseClass removes one class. What stays, as the privacy notice says, is the
// tax record: payments without their free-text notes, which no longer point at
// a person once the students and guardians are gone, and the class's invoices.
func eraseClass(ctx context.Context, d *mongo.Database, tenantID, userID string) error {
	// The class goes first: without it a stale access token can no longer sync
	// rows back in, because the change log refuses a missing class.
	if _, err := d.Collection("tenants").DeleteOne(ctx, bson.M{"_id": tenantID}); err != nil {
		return err
	}
	scope := bson.M{"tenant_id": tenantID}
	if err := eraseInbound(ctx, d, tenantID); err != nil {
		return err
	}
	for _, name := range classData {
		if _, err := d.Collection(name).DeleteMany(ctx, scope); err != nil {
			return err
		}
	}
	// The erase requests stay as the record that the request was carried out.
	if _, err := d.Collection("data_requests").DeleteMany(ctx, bson.M{"tenant_id": tenantID, "kind": bson.M{"$ne": AccountErase}}); err != nil {
		return err
	}
	if _, err := d.Collection("payments").UpdateMany(ctx, scope, bson.M{"$unset": bson.M{"note": ""}, "$set": bson.M{"erased_at": time.Now()}}); err != nil {
		return err
	}
	if err := Audit(ctx, d, "user:"+userID, "class_erased", tenantID, nil); err != nil {
		return err
	}
	_, err := d.Collection("memberships").DeleteMany(ctx, scope)
	return err
}

// eraseInbound removes WhatsApp messages from this class's guardians. Those
// are stored by phone, not by class, so a parent who is also in another class
// keeps theirs. It must run while the guardians still exist.
func eraseInbound(ctx context.Context, d *mongo.Database, tenantID string) error {
	guardians := d.Collection("guardians")
	var hashes []string
	if err := guardians.Distinct(ctx, "phone_hash", bson.M{"tenant_id": tenantID, "phone_hash": bson.M{"$type": "string"}}).Decode(&hashes); err != nil {
		return err
	}
	if len(hashes) == 0 {
		return nil
	}
	var shared []string
	if err := guardians.Distinct(ctx, "phone_hash", bson.M{"tenant_id": bson.M{"$ne": tenantID}, "phone_hash": bson.M{"$in": hashes}}).Decode(&shared); err != nil {
		return err
	}
	inShared := make(map[string]bool, len(shared))
	for _, h := range shared {
		inShared[h] = true
	}
	var only []string
	for _, h := range hashes {
		if !inShared[h] {
			only = append(only, h)
		}
	}
	if len(only) == 0 {
		return nil
	}
	_, err := d.Collection("inbound_messages").DeleteMany(ctx, bson.M{"from_hash": bson.M{"$in": only}})
	return err
}

// eraseUser removes the login itself, so signing in again starts a new account
// with nothing to sync back.
func eraseUser(ctx context.Context, d *mongo.Database, userID string) error {
	var user bson.M
	err := d.Collection("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&user)
	if db.NotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := d.Collection("otp_requests").DeleteMany(ctx, bson.M{"phone_hash": user["phone_hash"]}); err != nil {
		return err
	}
	if _, err := d.Collection("inbound_messages").DeleteMany(ctx, bson.M{"from_hash": user["phone_hash"]}); err != nil {
		return err
	}
	if _, err := d.Collection("refresh_tokens").DeleteMany(ctx, bson.M{"user_id": userID}); err != nil {
		return err
	}
	_, err = d.Collection("users").DeleteOne(ctx, bson.M{"_id": userID})
	return err
}
