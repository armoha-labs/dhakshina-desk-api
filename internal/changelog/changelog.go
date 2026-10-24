// Package changelog numbers accepted changes per class. The $inc on the
// tenant document runs inside the caller's transaction, so two writers to one
// class conflict and serialise: change numbers are always in commit order,
// and a phone that has seen number N has seen everything at or below N.
package changelog

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

type Entry struct {
	Table string
	ID    string
	Op    string // insert, upsert, delete
}

// Log must be called with a transaction ctx.
func Log(ctx context.Context, db *mongo.Database, tenantID string, entries ...Entry) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	var t bson.M
	err := db.Collection("tenants").FindOneAndUpdate(ctx, bson.M{"_id": tenantID},
		bson.M{"$inc": bson.M{"change_seq": len(entries)}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"change_seq": 1})).Decode(&t)
	if err != nil {
		return nil, fmt.Errorf("change log: %w", err)
	}
	base := val.IntOr(t["change_seq"], 0) - int64(len(entries))
	now := time.Now()
	rows := make([]any, len(entries))
	seqs := make([]int64, len(entries))
	for i, e := range entries {
		seqs[i] = base + int64(i) + 1
		rows[i] = bson.M{"tenant_id": tenantID, "seq": seqs[i], "table": e.Table, "doc_id": e.ID, "op": e.Op, "at": now}
	}
	_, err = db.Collection("change_log").InsertMany(ctx, rows)
	return seqs, err
}
