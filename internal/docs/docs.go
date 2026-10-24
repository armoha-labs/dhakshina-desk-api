// Package docs turns stored documents into the API shape: id instead of _id,
// encrypted fields decrypted, internal fields dropped, dates as strings.
package docs

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/fieldcrypt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

var internal = map[string]bool{"_id": true, "hlc": true, "phone_enc": true, "phone_hash": true, "dob_enc": true, "token_hash": true, "dedupe": true, "wa_id": true}

// ToAPI shapes a document of the given table for the phone.
func ToAPI(box *fieldcrypt.Box, table string, d bson.M) bson.M {
	if table == "tenants" {
		return TenantView(d)
	}
	out := bson.M{"id": d["_id"]}
	for k, v := range d {
		if internal[k] {
			continue
		}
		out[k] = plain(v)
	}
	if d["phone_enc"] != nil {
		out["phone"] = nullable(box.DecryptOr(d["phone_enc"]))
	}
	if d["dob_enc"] != nil {
		out["dob"] = nullable(box.DecryptOr(d["dob_enc"]))
	} else if _, had := d["dob_enc"]; had {
		out["dob"] = nil
	}
	return out
}

// TenantView is the public shape of a class, as the app sees it.
func TenantView(t bson.M) bson.M {
	return bson.M{
		"id":                t["_id"],
		"tenant_id":         t["_id"],
		"name":              t["name"],
		"owner_name":        orEmpty(t["owner_name"]),
		"upi_id":            orEmpty(t["upi_id"]),
		"upi_name":          orEmpty(t["upi_name"]),
		"city":              t["city"],
		"receipt_prefix":    orDefault(t["receipt_prefix"], "DD"),
		"grievance_contact": t["grievance_contact"],
		"created_at":        plain(t["created_at"]),
		"updated_at":        plain(t["updated_at"]),
		"deleted":           false,
	}
}

func plain(v any) any {
	switch x := v.(type) {
	case time.Time:
		return val.ISO(x)
	case bson.DateTime:
		return val.ISO(x.Time())
	case bson.M:
		out := bson.M{}
		for k, e := range x {
			out[k] = plain(e)
		}
		return out
	case bson.D:
		out := bson.M{}
		for _, e := range x {
			out[e.Key] = plain(e.Value)
		}
		return out
	case bson.A:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	}
	return v
}

// Plain converts stored values (dates, nested documents) for JSON output.
func Plain(v any) any { return plain(v) }

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func orDefault(v any, d string) any {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
