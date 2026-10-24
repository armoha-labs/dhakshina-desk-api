package identity

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/privacy"
)

// Routes mounts the auth calls and DELETE /me under /v1.
func (s *Service) Routes(r chi.Router) {
	r.Method("POST", "/auth/otp/start", httpx.Handler(s.start))
	r.Method("POST", "/auth/otp/verify", httpx.Handler(s.verify))
	r.Method("POST", "/auth/refresh", httpx.Handler(s.refresh))
	r.Method("POST", "/auth/switch", httpx.Handler(s.switchTenant))
	r.Method("POST", "/auth/logout", httpx.Handler(s.logout))
	r.Method("DELETE", "/me", httpx.Handler(s.deleteMe))
}

func (s *Service) start(w http.ResponseWriter, r *http.Request) error {
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	phone, ok := b["phone"].(string)
	if !ok {
		return httpx.Err(httpx.BadRequest, "Enter your WhatsApp number.")
	}
	out, err := s.StartOTP(r.Context(), phone)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, out)
	return nil
}

func (s *Service) verify(w http.ResponseWriter, r *http.Request) error {
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	reqID, code := httpx.Str(b["request_id"], 64), httpx.Str(b["code"], 10)
	if reqID == "" || code == "" {
		return httpx.Err(httpx.BadRequest, "Enter the 6-digit code.")
	}
	uid, err := s.VerifyOTP(r.Context(), reqID, code)
	if err != nil {
		return err
	}
	out, err := s.IssueSession(r.Context(), uid, "", httpx.Str(b["device_id"], 64))
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, out)
	return nil
}

func (s *Service) refresh(w http.ResponseWriter, r *http.Request) error {
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	tok := httpx.Str(b["refresh_token"], 200)
	if tok == "" {
		return httpx.Err(httpx.Unauthorized, "Please sign in again.")
	}
	out, err := s.Refresh(r.Context(), tok)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, out)
	return nil
}

func (s *Service) switchTenant(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Auth(r)
	if err != nil {
		return err
	}
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	tid := httpx.Str(b["tenant_id"], 64)
	if tid == "" {
		return httpx.Err(httpx.BadRequest, "Pick a class.")
	}
	out, err := s.IssueSession(r.Context(), p.UserID, tid, "")
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, out)
	return nil
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) error {
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	if tok := httpx.Str(b["refresh_token"], 200); tok != "" {
		if err := s.Revoke(r.Context(), tok); err != nil {
			return err
		}
	}
	httpx.NoContent(w)
	return nil
}

// deleteMe is the Play-required account deletion. It queues the DPDP erasure
// flow (financial records are kept as long as tax law requires) and signs
// the user out everywhere.
func (s *Service) deleteMe(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Auth(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var tid any
	if p.TenantID != "" {
		tid = p.TenantID
	}
	now := time.Now()
	if _, err := s.M.C("data_requests").InsertOne(ctx, bson.M{"_id": ids.UUIDv7(), "tenant_id": tid, "user_id": p.UserID, "kind": "account_erase", "status": "open", "created_at": now}); err != nil {
		return err
	}
	if _, err := s.M.C("refresh_tokens").UpdateMany(ctx, bson.M{"user_id": p.UserID}, bson.M{"$set": bson.M{"revoked": true, "revoked_at": now}}); err != nil {
		return err
	}
	if err := privacy.Audit(ctx, s.M.DB, "user:"+p.UserID, "account_delete_requested", p.TenantID, nil); err != nil {
		return err
	}
	httpx.JSON(w, 202, bson.M{"status": "queued"})
	return nil
}
