// Package db holds the MongoDB clients: one for class data and, when
// MONGODB_ADMIN_URI is set, a second for the admin console whose database
// user can read only platform collections.
package db

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

type Mongo struct {
	Client      *mongo.Client
	DB          *mongo.Database
	adminClient *mongo.Client
	AdminDB     *mongo.Database
}

func Connect(ctx context.Context, uri, adminURI, name string) (*Mongo, error) {
	c, err := mongo.Connect(options.Client().ApplyURI(uri).
		SetAppName("dhakshina-desk-api").
		SetMaxPoolSize(20).
		SetServerSelectionTimeout(5 * time.Second))
	if err != nil {
		return nil, err
	}
	m := &Mongo{Client: c, DB: c.Database(name)}
	m.AdminDB = m.DB
	if adminURI != "" && adminURI != uri {
		ac, err := mongo.Connect(options.Client().ApplyURI(adminURI).SetAppName("dhakshina-desk-admin").SetMaxPoolSize(3))
		if err != nil {
			return nil, err
		}
		m.adminClient = ac
		m.AdminDB = ac.Database(name)
	}
	return m, nil
}

func (m *Mongo) Close(ctx context.Context) {
	_ = m.Client.Disconnect(ctx)
	if m.adminClient != nil {
		_ = m.adminClient.Disconnect(ctx)
	}
}

func (m *Mongo) C(name string) *mongo.Collection { return m.DB.Collection(name) }

// Tx runs fn in one multi-document transaction with majority writes and
// snapshot reads. fn may be retried on transient errors, so it must reset
// any state it collects. Use the ctx it is given for every operation.
func (m *Mongo) Tx(ctx context.Context, fn func(ctx context.Context) error) error {
	s, err := m.Client.StartSession()
	if err != nil {
		return err
	}
	defer s.EndSession(ctx)
	opts := options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority())
	_, err = s.WithTransaction(ctx, func(ctx context.Context) (any, error) { return nil, fn(ctx) }, opts)
	return err
}

// IsDup reports a unique index violation.
func IsDup(err error) bool { return mongo.IsDuplicateKeyError(err) }

// NotFound reports a FindOne with no document.
func NotFound(err error) bool { return errors.Is(err, mongo.ErrNoDocuments) }
