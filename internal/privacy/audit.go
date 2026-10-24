// Package privacy holds the append-only audit log. Consent and data-rights
// requests from parents live in the collections module next to the family
// page that creates them.
package privacy

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
)

// Audit appends one entry: staff access, plan changes, exports, deletions.
func Audit(ctx context.Context, db *mongo.Database, actor, action, tenantID string, detail bson.M) error {
	var tid any
	if tenantID != "" {
		tid = tenantID
	}
	if detail == nil {
		detail = bson.M{}
	}
	_, err := db.Collection("audit_log").InsertOne(ctx, bson.M{
		"_id": ids.UUIDv7(), "tenant_id": tid, "actor": actor, "action": action, "detail": detail, "at": time.Now(),
	})
	return err
}
