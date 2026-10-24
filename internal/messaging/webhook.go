package messaging

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/jobs"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

// Routes mounts Meta's webhook. The handler only checks the signature,
// stores the raw event and returns at once; a job processes it, which keeps
// Meta's retries away and survives bursts.
func (w *Worker) Routes(r chi.Router) {
	r.Get("/webhooks/whatsapp", w.verifyHook)
	r.Method("POST", "/webhooks/whatsapp", httpx.Handler(w.receive))
}

func (w *Worker) verifyHook(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") == "subscribe" && w.Cfg.WhatsAppVerifyToken != "" && q.Get("hub.verify_token") == w.Cfg.WhatsAppVerifyToken {
		rw.Header().Set("Content-Type", "text/plain")
		_, _ = rw.Write([]byte(q.Get("hub.challenge")))
		return
	}
	rw.WriteHeader(http.StatusForbidden)
}

func (w *Worker) receive(rw http.ResponseWriter, r *http.Request) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return httpx.Err(httpx.BadRequest, "Could not read the event.")
	}
	if !w.WA.VerifySignature(raw, r.Header.Get("X-Hub-Signature-256")) {
		return httpx.Err(httpx.Unauthorized, "Bad signature.")
	}
	id := ids.UUIDv7()
	ctx := r.Context()
	err = w.M.Tx(ctx, func(ctx context.Context) error {
		if _, err := w.M.C("wa_events").InsertOne(ctx, bson.M{"_id": id, "raw": string(raw), "received_at": time.Now()}); err != nil {
			return err
		}
		return jobs.Enqueue(ctx, w.M.DB, jobs.Job{Kind: jobs.WAEvent, Ref: id})
	})
	if err != nil {
		return err
	}
	httpx.JSON(rw, 200, bson.M{"ok": true})
	return nil
}

type waEvent struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Statuses []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Errors []struct {
						Title string `json:"title"`
					} `json:"errors"`
				} `json:"statuses"`
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text *struct {
						Body string `json:"body"`
					} `json:"text"`
					Button *struct {
						Payload string `json:"payload"`
						Text    string `json:"text"`
					} `json:"button"`
					Interactive *struct {
						ButtonReply *struct {
							ID string `json:"id"`
						} `json:"button_reply"`
					} `json:"interactive"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

var statusRank = map[string]int{"queued": 0, "sent": 1, "delivered": 2, "read": 3}

func (w *Worker) processEvent(ctx context.Context, j bson.M) error {
	var ev bson.M
	if err := w.M.C("wa_events").FindOne(ctx, bson.M{"_id": j["ref"]}).Decode(&ev); err != nil {
		if db.NotFound(err) {
			return jobs.Finish(ctx, w.M.DB, j["_id"], "event gone")
		}
		return err
	}
	var e waEvent
	if err := json.Unmarshal([]byte(val.Str(ev["raw"])), &e); err != nil {
		return jobs.Fail(ctx, w.M.DB, j["_id"], "bad event json")
	}
	for _, en := range e.Entry {
		for _, ch := range en.Changes {
			for _, st := range ch.Value.Statuses {
				if err := w.applyStatus(ctx, st.ID, st.Status, st.Errors); err != nil {
					return err
				}
			}
			for _, m := range ch.Value.Messages {
				text, payload := "", ""
				if m.Text != nil {
					text = m.Text.Body
				}
				if m.Button != nil {
					payload, text = m.Button.Payload, m.Button.Text
				}
				if m.Interactive != nil && m.Interactive.ButtonReply != nil {
					payload = m.Interactive.ButtonReply.ID
				}
				if err := w.inbound(ctx, "+"+strings.TrimPrefix(m.From, "+"), m.ID, m.Type, text, payload); err != nil {
					return err
				}
			}
		}
	}
	return jobs.Finish(ctx, w.M.DB, j["_id"], "processed")
}

// applyStatus moves a message forward (sent → delivered → read) or to
// failed, and logs it so the teacher sees "Delivered" or "Read".
func (w *Worker) applyStatus(ctx context.Context, waID, status string, errs []struct {
	Title string `json:"title"`
}) error {
	var m bson.M
	if err := w.M.C("messages").FindOne(ctx, bson.M{"wa_id": waID}).Decode(&m); err != nil {
		if db.NotFound(err) {
			return nil // a login code or owner notice, not a class message
		}
		return err
	}
	cur := val.Str(m["status"])
	set := bson.M{}
	switch status {
	case "failed":
		if cur == "read" || cur == "failed" {
			return nil
		}
		set["status"] = "failed"
		if len(errs) > 0 {
			set["error"] = errs[0].Title
		}
	case "sent", "delivered", "read":
		if r, ok := statusRank[cur]; !ok || statusRank[status] <= r {
			return nil
		}
		set["status"] = status
		set[status+"_at"] = time.Now()
	default:
		return nil
	}
	return w.setStatus(ctx, val.Str(m["tenant_id"]), val.Str(m["_id"]), set)
}

var keywords = map[string]bool{"pay": true, "balance": true, "dues": true, "due": true, "fee": true, "fees": true, "receipt": true, "link": true}

// inbound answers what a parent writes to the shared number: consent
// buttons, and "pay"/"balance"/"receipt" with their family page link.
// Anything else gets one pointer a day to message the teacher directly.
func (w *Worker) inbound(ctx context.Context, phone, waID, kind, text, payload string) error {
	hash := w.Box.BlindIndex(phone)
	if r := []rune(text); len(r) > 1000 {
		text = string(r[:1000])
	}
	now := time.Now()
	if _, err := w.M.C("inbound_messages").InsertOne(ctx, bson.M{"_id": ids.UUIDv7(), "wa_id": waID, "from_hash": hash, "type": kind, "text": text, "payload": payload, "at": now}); err != nil && !db.IsDup(err) {
		return err
	} else if db.IsDup(err) {
		return nil // Meta redelivered the same message
	}

	if strings.HasPrefix(payload, "consent:") {
		p := strings.Split(payload, ":")
		if len(p) == 4 && (p[1] == "given" || p[1] == "withdrawn") {
			var g bson.M
			err := w.M.C("guardians").FindOne(ctx, bson.M{"_id": p[3], "tenant_id": p[2], "phone_hash": hash, "deleted": bson.M{"$ne": true}}).Decode(&g)
			if err == nil {
				if err := w.Family.SetConsent(ctx, p[2], p[3], p[1], "whatsapp", ""); err != nil {
					return err
				}
				reply := "Thank you. Your consent is saved. You can change it any time from your fee page."
				if p[1] == "withdrawn" {
					reply = "Understood. Your class will not send you fee messages here."
				}
				return w.reply(ctx, phone, reply)
			} else if !db.NotFound(err) {
				return err
			}
		}
		return nil
	}

	word := strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!?"))
	if keywords[word] {
		cur, err := w.M.C("guardians").Find(ctx, bson.M{"phone_hash": hash, "deleted": bson.M{"$ne": true}})
		if err != nil {
			return err
		}
		var gs []bson.M
		if err := cur.All(ctx, &gs); err != nil {
			return err
		}
		var lines []string
		for _, g := range gs {
			var t bson.M
			if err := w.M.C("tenants").FindOne(ctx, bson.M{"_id": g["tenant_id"], "status": bson.M{"$ne": "suspended"}}).Decode(&t); err != nil {
				continue
			}
			tok, _ := w.Links.Parent(val.Str(g["tenant_id"]), val.Str(g["_id"]))
			lines = append(lines, val.Str(t["name"])+": "+w.WebURL("/p/"+tok))
		}
		if len(lines) > 0 {
			return w.reply(ctx, phone, "Your fee page, with dues, receipts and Pay by UPI:\n"+strings.Join(lines, "\n"))
		}
	}
	n, err := w.M.C("inbound_messages").CountDocuments(ctx, bson.M{"from_hash": hash, "auto_replied": true, "at": bson.M{"$gt": now.Add(-24 * time.Hour)}})
	if err != nil || n > 0 {
		return err
	}
	_, _ = w.M.C("inbound_messages").UpdateOne(ctx, bson.M{"wa_id": waID}, bson.M{"$set": bson.M{"auto_replied": true}})
	return w.reply(ctx, phone, "This number sends fee messages for classes on Dhakshina Desk and is not read by your teacher. Please message your teacher directly. Reply \"pay\" for your fee page.")
}

func (w *Worker) reply(ctx context.Context, phone, body string) error {
	if _, err := w.WA.SendText(ctx, phone, body); err != nil {
		slog.Error("whatsapp_reply_failed", "err", err)
	}
	return nil
}
