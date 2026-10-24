// Package store is tenant scoping in one place. MongoDB has no row-level
// security, so every read and write of class data goes through a handle that
// stamps tenant_id on the filter or document. Handlers never build a tenant
// filter themselves.
package store

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type Scoped struct {
	db       *mongo.Database
	TenantID string
}

func For(db *mongo.Database, tenantID string) *Scoped {
	if tenantID == "" {
		panic("store.For needs a tenant id")
	}
	return &Scoped{db: db, TenantID: tenantID}
}

func (s *Scoped) scope(f bson.M) bson.M {
	out := bson.M{}
	for k, v := range f {
		out[k] = v
	}
	out["tenant_id"] = s.TenantID
	return out
}

func (s *Scoped) c(name string) *mongo.Collection { return s.db.Collection(name) }

// Find returns all matching documents.
func (s *Scoped) Find(ctx context.Context, name string, f bson.M, opts ...options.Lister[options.FindOptions]) ([]bson.M, error) {
	cur, err := s.c(name).Find(ctx, s.scope(f), opts...)
	if err != nil {
		return nil, err
	}
	var out []bson.M
	err = cur.All(ctx, &out)
	return out, err
}

// FindOne returns the document or nil when there is none.
func (s *Scoped) FindOne(ctx context.Context, name string, f bson.M) (bson.M, error) {
	var out bson.M
	err := s.c(name).FindOne(ctx, s.scope(f)).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return out, err
}

func (s *Scoped) Count(ctx context.Context, name string, f bson.M) (int64, error) {
	return s.c(name).CountDocuments(ctx, s.scope(f))
}

func (s *Scoped) InsertOne(ctx context.Context, name string, doc bson.M) error {
	_, err := s.c(name).InsertOne(ctx, s.scope(doc))
	return err
}

func (s *Scoped) UpdateOne(ctx context.Context, name string, f bson.M, update bson.M, upsert bool) (*mongo.UpdateResult, error) {
	return s.c(name).UpdateOne(ctx, s.scope(f), update, options.UpdateOne().SetUpsert(upsert))
}

func (s *Scoped) UpdateMany(ctx context.Context, name string, f bson.M, update bson.M) (*mongo.UpdateResult, error) {
	return s.c(name).UpdateMany(ctx, s.scope(f), update)
}

func (s *Scoped) Aggregate(ctx context.Context, name string, pipeline bson.A) ([]bson.M, error) {
	p := append(bson.A{bson.M{"$match": bson.M{"tenant_id": s.TenantID}}}, pipeline...)
	cur, err := s.c(name).Aggregate(ctx, p)
	if err != nil {
		return nil, err
	}
	var out []bson.M
	err = cur.All(ctx, &out)
	return out, err
}

// Tenant returns the class document, or nil.
func (s *Scoped) Tenant(ctx context.Context) (bson.M, error) {
	var out bson.M
	err := s.c("tenants").FindOne(ctx, bson.M{"_id": s.TenantID}).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return out, err
}

// UpdateTenant applies an update to the class document and returns it after.
func (s *Scoped) UpdateTenant(ctx context.Context, update bson.M) (bson.M, error) {
	var out bson.M
	err := s.c("tenants").FindOneAndUpdate(ctx, bson.M{"_id": s.TenantID}, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return out, err
}
