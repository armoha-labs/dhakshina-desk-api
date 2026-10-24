package syncer

import (
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/fieldcrypt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

// Reject is a change the server refuses; it goes back in rejected[] and the
// app shows it to the teacher instead of retrying.
type Reject struct{ Code, Message string }

func (r *Reject) Error() string { return r.Code + ": " + r.Message }

func reject(code, msg string) *Reject { return &Reject{code, msg} }

func oneOf(v any, allowed ...string) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	for _, a := range allowed {
		if s == a {
			return s, true
		}
	}
	return "", false
}

func text(v any, max int) (string, bool) { return val.Text(v, max) }

// optDate: null clears, a date sets, anything else leaves the field alone.
func optDate(m map[string]any, k string, out bson.M) {
	v, present := m[k]
	if !present {
		return
	}
	if v == nil {
		out[k] = nil
	} else if d, ok := val.Date(v); ok {
		out[k] = d
	}
}

func uuidField(m map[string]any, k string, out bson.M) {
	if ids.IsUUID(m[k]) {
		out[k] = m[k]
	}
}

func uuidOrNil(v any) any {
	if ids.IsUUID(v) {
		return v
	}
	return nil
}

// clean applies the field whitelist: the app can only write these. Everything
// else on a stored document (tenant_id, receipt_no, consent, delivery status)
// is server-owned.
func clean(box *fieldcrypt.Box, table string, d map[string]any, existing bson.M) (bson.M, error) {
	out := bson.M{}
	if d == nil {
		d = map[string]any{}
	}
	isNew := existing == nil
	switch table {
	case "batches":
		if s, ok := text(d["name"], 80); ok {
			out["name"] = s
		}
		if s, ok := text(d["schedule"], 120); ok {
			out["schedule"] = s
		}
		uuidField(d, "fee_plan_id", out)
		if v, present := d["due_day"]; present {
			n, ok := val.Int(v)
			if !ok || n < 1 || n > 28 {
				return nil, reject("bad_request", "Due day must be 1 to 28.")
			}
			out["due_day"] = n
		}
		if b, ok := d["archived"].(bool); ok {
			out["archived"] = b
		}
		if isNew && val.Str(out["name"]) == "" {
			return nil, reject("bad_request", "A batch needs a name.")
		}
	case "fee_plans":
		if s, ok := text(d["name"], 80); ok {
			out["name"] = s
		}
		if v, present := d["cycle"]; present {
			c, ok := oneOf(v, "monthly", "quarterly", "term", "per_class", "one_time")
			if !ok {
				return nil, reject("bad_request", "Unknown fee cycle.")
			}
			out["cycle"] = c
		}
		if v, present := d["versions"]; present {
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				return nil, reject("bad_request", "A fee plan needs an amount.")
			}
			type ver struct {
				amount int64
				from   string
			}
			vs := make([]ver, 0, len(list))
			for _, x := range list {
				m := val.M(x)
				amt, ok1 := val.Paise(m["amount"])
				from, ok2 := val.Date(m["effective_from"])
				if !ok1 || !ok2 {
					return nil, reject("bad_request", "Each fee version needs an amount and a start date.")
				}
				vs = append(vs, ver{amt, from})
			}
			sort.SliceStable(vs, func(i, j int) bool { return vs[i].from < vs[j].from })
			// Past versions never change: an edit may only add versions after them.
			today := time.Now().In(val.IST).Format("2006-01-02")
			if existing != nil {
				for _, o := range asList(existing["versions"]) {
					om := val.M(o)
					of := val.Str(om["effective_from"])
					if of > today {
						continue
					}
					oa := val.IntOr(om["amount"], -1)
					found := false
					for _, v := range vs {
						if v.from == of && v.amount == oa {
							found = true
						}
					}
					if !found {
						return nil, reject("bad_request", "Past fees cannot be changed. Add a new fee from a future date.")
					}
				}
			}
			arr := bson.A{}
			for _, v := range vs {
				arr = append(arr, bson.M{"amount": v.amount, "effective_from": v.from})
			}
			out["versions"] = arr
		}
		if isNew && (val.Str(out["name"]) == "" || out["cycle"] == nil || out["versions"] == nil) {
			return nil, reject("bad_request", "A fee plan needs a name, cycle and amount.")
		}
	case "guardians":
		if s, ok := text(d["name"], 80); ok {
			out["name"] = s
		}
		if v, present := d["phone"]; present {
			raw, _ := v.(string)
			phone, ok := fieldcrypt.NormalisePhone(raw)
			if !ok {
				return nil, reject("bad_request", "That phone number does not look right.")
			}
			out["phone_enc"] = box.Encrypt(phone)
			out["phone_hash"] = box.BlindIndex(phone)
		}
		if isNew {
			if out["phone_enc"] == nil {
				return nil, reject("bad_request", "A guardian needs a phone number.")
			}
			out["consent"] = "pending"
		}
	case "students":
		if s, ok := text(d["name"], 80); ok {
			out["name"] = s
		}
		uuidField(d, "guardian_id", out)
		if v, present := d["dob"]; present {
			if v == nil {
				out["dob_enc"] = nil
			} else if dob, ok := val.Date(v); ok {
				out["dob_enc"] = box.Encrypt(dob)
			}
		}
		if isNew && (val.Str(out["name"]) == "" || out["guardian_id"] == nil) {
			return nil, reject("bad_request", "A student needs a name and a guardian.")
		}
	case "enrolments":
		for _, k := range []string{"student_id", "batch_id", "fee_plan_id"} {
			uuidField(d, k, out)
		}
		if v, present := d["status"]; present {
			st, ok := oneOf(v, "active", "paused", "left")
			if !ok {
				return nil, reject("bad_request", "Unknown enrolment status.")
			}
			out["status"] = st
		}
		for _, k := range []string{"joined_on", "left_on", "paused_from", "paused_until"} {
			optDate(d, k, out)
		}
		if v, present := d["fee_override"]; present {
			if v == nil {
				out["fee_override"] = nil
			} else {
				o := val.M(v)
				amt, ok := val.Int(o["amount"])
				if !ok || amt < 0 {
					return nil, reject("bad_request", "Override needs an amount.")
				}
				reason, _ := text(o["reason"], 200)
				if reason == "" {
					return nil, reject("bad_request", "Override needs a reason.")
				}
				ov := bson.M{"amount": amt, "reason": reason, "from": nil, "until": nil}
				if f, ok := val.Date(o["from"]); ok {
					ov["from"] = f
				}
				if u, ok := val.Date(o["until"]); ok {
					ov["until"] = u
				}
				out["fee_override"] = ov
			}
		}
		if isNew && (out["student_id"] == nil || out["batch_id"] == nil || out["fee_plan_id"] == nil || out["joined_on"] == nil) {
			return nil, reject("bad_request", "An enrolment needs a student, batch, fee plan and join date.")
		}
		if isNew && out["status"] == nil {
			out["status"] = "active"
		}
	case "payment_claims":
		if isNew {
			return nil, reject("not_found", "That payment claim is not on the server.")
		}
		st, ok := oneOf(d["status"], "pending", "confirmed", "rejected")
		if !ok {
			return nil, reject("bad_request", "Unknown claim status.")
		}
		out["status"] = st
		if st == "pending" {
			out["resolved_at"] = nil
		} else {
			out["resolved_at"] = time.Now()
		}
	case "charges":
		if !ids.IsUUID(d["enrolment_id"]) {
			return nil, reject("bad_request", "A charge needs an enrolment.")
		}
		amt, ok := val.Paise(d["amount"])
		if !ok {
			return nil, reject("bad_request", "A charge needs a positive amount.")
		}
		kind, ok := oneOf(d["kind"], "fee", "opening", "item", "late_fee")
		if !ok {
			return nil, reject("bad_request", "Unknown charge kind.")
		}
		cycle, _ := text(d["cycle"], 20)
		if cycle == "" {
			return nil, reject("bad_request", "A charge needs a cycle.")
		}
		due, ok := val.Date(d["due_on"])
		if !ok {
			return nil, reject("bad_request", "A charge needs a due date.")
		}
		label, _ := text(d["label"], 80)
		out = bson.M{"enrolment_id": d["enrolment_id"], "amount": amt, "kind": kind, "cycle": cycle, "due_on": due, "label": label, "dedupe": kind != "item"}
	case "payments":
		if !ids.IsUUID(d["enrolment_id"]) {
			return nil, reject("bad_request", "A payment needs an enrolment.")
		}
		amt, ok := val.Paise(d["amount"])
		if !ok {
			return nil, reject("bad_request", "A payment needs a positive amount.")
		}
		mode, ok := oneOf(d["mode"], "cash", "upi", "bank", "gateway")
		if !ok {
			return nil, reject("bad_request", "Unknown payment mode.")
		}
		paid, ok := val.Date(d["paid_on"])
		if !ok {
			return nil, reject("bad_request", "A payment needs a date.")
		}
		var utr, note any
		if s, _ := text(d["utr"], 40); s != "" {
			utr = s
		}
		if s, ok := text(d["note"], 200); ok {
			note = s
		}
		out = bson.M{"enrolment_id": d["enrolment_id"], "amount": amt, "mode": mode, "paid_on": paid, "utr": utr,
			"claim_id": uuidOrNil(d["claim_id"]), "note": note, "receipt_no": nil}
	case "adjustments":
		if !ids.IsUUID(d["enrolment_id"]) {
			return nil, reject("bad_request", "An adjustment needs an enrolment.")
		}
		amt, ok := val.Int(d["amount"])
		if !ok || amt == 0 {
			return nil, reject("bad_request", "An adjustment needs an amount.")
		}
		kind, ok := oneOf(d["kind"], "discount", "waiver", "opening_advance", "write_off", "reversal", "correction")
		if !ok {
			return nil, reject("bad_request", "Unknown adjustment kind.")
		}
		reason, _ := text(d["reason"], 200)
		if reason == "" {
			return nil, reject("bad_request", "An adjustment needs a reason.")
		}
		out = bson.M{"enrolment_id": d["enrolment_id"], "amount": amt, "kind": kind, "reason": reason, "reverses_id": uuidOrNil(d["reverses_id"])}
	case "messages":
		kind, ok := oneOf(d["kind"], "reminder", "receipt", "fee_notice")
		if !ok {
			return nil, reject("bad_request", "Unknown message kind.")
		}
		if !ids.IsUUID(d["guardian_id"]) {
			return nil, reject("bad_request", "A message needs a guardian.")
		}
		out = bson.M{"kind": kind, "guardian_id": d["guardian_id"], "enrolment_id": uuidOrNil(d["enrolment_id"]),
			"payment_id": uuidOrNil(d["payment_id"]), "status": "queued"}
	}
	return out, nil
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
