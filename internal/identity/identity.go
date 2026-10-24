// Package identity is WhatsApp OTP login and the token pair from the API
// contract: a one-hour access JWT and a 180-day refresh token rotated on use.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/fieldcrypt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/ids"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/jwt"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/whatsapp"
)

const (
	AccessTTL        = time.Hour
	RefreshTTL       = 180 * 24 * time.Hour
	otpTTL           = 5 * time.Minute
	otpResend        = 30
	otpStartsPerHour = 5
	otpMaxAttempts   = 5
)

type Service struct{ *app.App }

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// StartOTP creates a login code and sends it on WhatsApp (production) or
// returns it as dev_code (anything else).
func (s *Service) StartOTP(ctx context.Context, rawPhone string) (bson.M, error) {
	phone, ok := fieldcrypt.NormalisePhone(rawPhone)
	if !ok {
		return nil, httpx.Err(httpx.BadRequest, "Enter a valid mobile number.")
	}
	hash := s.Box.BlindIndex(phone)
	otps := s.M.C("otp_requests")
	recent, err := otps.CountDocuments(ctx, bson.M{"phone_hash": hash, "created_at": bson.M{"$gt": time.Now().Add(-time.Hour)}})
	if err != nil {
		return nil, err
	}
	if recent >= otpStartsPerHour {
		return nil, httpx.Err(httpx.OTPRateLimited, "Too many codes asked for. Try again in an hour.")
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	code := fmt.Sprintf("%06d", n.Int64())
	id := ids.UUIDv7()
	now := time.Now()
	if _, err := otps.InsertOne(ctx, bson.M{
		"_id": id, "phone_hash": hash, "phone_enc": s.Box.Encrypt(phone),
		"code_hash": sha(id + ":" + code), "attempts": 0, "used": false,
		"created_at": now, "expires_at": now.Add(otpTTL),
	}); err != nil {
		return nil, err
	}
	out := bson.M{"request_id": id, "expires_in": int(otpTTL.Seconds()), "resend_in": otpResend}
	if s.Cfg.IsProd() {
		// Authentication template with a copy-code button. SMS fallback is a later step.
		if _, err := s.WA.SendTemplate(ctx, phone, whatsapp.Template{Name: "dd_login_code", Params: []string{code}, ButtonURLSuffix: code}); err != nil {
			slog.Error("otp_send_failed", "err", err)
		}
	} else {
		out["dev_code"] = code
	}
	return out, nil
}

// VerifyOTP checks the code and returns the user id, creating the user on
// first login.
func (s *Service) VerifyOTP(ctx context.Context, requestID, code string) (string, error) {
	otps := s.M.C("otp_requests")
	var req bson.M
	if err := otps.FindOne(ctx, bson.M{"_id": requestID}).Decode(&req); err != nil {
		if db.NotFound(err) {
			return "", httpx.Err(httpx.OTPInvalid, "That code is not right.")
		}
		return "", err
	}
	if val.Bool(req["used"]) {
		return "", httpx.Err(httpx.OTPInvalid, "That code is not right.")
	}
	if exp, _ := val.Time(req["expires_at"]); exp.Before(time.Now()) {
		return "", httpx.Err(httpx.OTPExpired, "That code has expired. Ask for a new one.")
	}
	if val.IntOr(req["attempts"], 0) >= otpMaxAttempts {
		return "", httpx.Err(httpx.OTPInvalid, "Too many tries. Ask for a new code.")
	}
	want := val.Str(req["code_hash"])
	got := sha(requestID + ":" + strings.TrimSpace(code))
	if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		_, _ = otps.UpdateOne(ctx, bson.M{"_id": requestID}, bson.M{"$inc": bson.M{"attempts": 1}})
		return "", httpx.Err(httpx.OTPInvalid, "That code is not right.")
	}
	res, err := otps.UpdateOne(ctx, bson.M{"_id": requestID, "used": false}, bson.M{"$set": bson.M{"used": true}})
	if err != nil {
		return "", err
	}
	if res.ModifiedCount != 1 {
		return "", httpx.Err(httpx.OTPInvalid, "That code is not right.")
	}
	users := s.M.C("users")
	var user bson.M
	err = users.FindOne(ctx, bson.M{"phone_hash": req["phone_hash"]}).Decode(&user)
	if db.NotFound(err) {
		id := ids.UUIDv7()
		_, err = users.InsertOne(ctx, bson.M{"_id": id, "phone_hash": req["phone_hash"], "phone_enc": req["phone_enc"], "name": "", "created_at": time.Now()})
		if db.IsDup(err) { // two verifies racing for a new user
			err = users.FindOne(ctx, bson.M{"phone_hash": req["phone_hash"]}).Decode(&user)
			return val.Str(user["_id"]), err
		}
		return id, err
	}
	if err != nil {
		return "", err
	}
	return val.Str(user["_id"]), nil
}

type Membership struct {
	TenantID   string `json:"tenant_id"`
	TenantName string `json:"tenant_name"`
	Role       string `json:"role"`
}

func (s *Service) memberships(ctx context.Context, userID string) ([]Membership, error) {
	cur, err := s.M.C("memberships").Find(ctx, bson.M{"user_id": userID, "removed": bson.M{"$ne": true}})
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := []Membership{}
	if len(rows) == 0 {
		return out, nil
	}
	tids := make([]string, len(rows))
	for i, r := range rows {
		tids[i] = val.Str(r["tenant_id"])
	}
	cur, err = s.M.C("tenants").Find(ctx, bson.M{"_id": bson.M{"$in": tids}}, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	var ts []bson.M
	if err := cur.All(ctx, &ts); err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, t := range ts {
		names[val.Str(t["_id"])] = val.Str(t["name"])
	}
	for _, r := range rows {
		tid := val.Str(r["tenant_id"])
		out = append(out, Membership{TenantID: tid, TenantName: names[tid], Role: val.Str(r["role"])})
	}
	return out, nil
}

// IssueSession makes a fresh access and refresh token. With no tenantID, a
// user with exactly one class gets a token scoped to it.
func (s *Service) IssueSession(ctx context.Context, userID, tenantID, deviceID string) (bson.M, error) {
	ms, err := s.memberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	var scope *Membership
	if tenantID != "" {
		for i := range ms {
			if ms[i].TenantID == tenantID {
				scope = &ms[i]
			}
		}
		if scope == nil {
			return nil, httpx.Err(httpx.Forbidden, "You are not a member of that class.")
		}
	} else if len(ms) == 1 {
		scope = &ms[0]
	}
	var user bson.M
	if err := s.M.C("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&user); err != nil {
		if db.NotFound(err) {
			return nil, httpx.Err(httpx.Unauthorized, "Please sign in again.")
		}
		return nil, err
	}
	claims := jwt.Claims{"sub": userID}
	var scopeTenant any
	if scope != nil {
		claims["tid"], claims["role"] = scope.TenantID, scope.Role
		scopeTenant = scope.TenantID
	}
	access := jwt.Sign(claims, s.Cfg.JWTSecret, AccessTTL)
	rb := make([]byte, 32)
	_, _ = rand.Read(rb)
	refresh := "r_" + base64.RawURLEncoding.EncodeToString(rb)
	var device any
	if deviceID != "" {
		device = deviceID
	}
	now := time.Now()
	if _, err := s.M.C("refresh_tokens").InsertOne(ctx, bson.M{
		"_id": sha(refresh), "user_id": userID, "tenant_id": scopeTenant, "device_id": device,
		"revoked": false, "created_at": now, "expires_at": now.Add(RefreshTTL),
	}); err != nil {
		return nil, err
	}
	return bson.M{
		"access_token":  access,
		"refresh_token": refresh,
		"expires_in":    int(AccessTTL.Seconds()),
		"user":          bson.M{"id": userID, "phone": s.Box.DecryptOr(user["phone_enc"]), "name": val.Str(user["name"])},
		"memberships":   ms,
	}, nil
}

// Refresh rotates a refresh token: the old one is revoked as the new one is issued.
func (s *Service) Refresh(ctx context.Context, token string) (bson.M, error) {
	var row bson.M
	err := s.M.C("refresh_tokens").FindOneAndUpdate(ctx,
		bson.M{"_id": sha(token), "revoked": false},
		bson.M{"$set": bson.M{"revoked": true, "revoked_at": time.Now()}}).Decode(&row)
	if db.NotFound(err) {
		return nil, httpx.Err(httpx.Unauthorized, "Please sign in again.")
	}
	if err != nil {
		return nil, err
	}
	if exp, _ := val.Time(row["expires_at"]); exp.Before(time.Now()) {
		return nil, httpx.Err(httpx.Unauthorized, "Please sign in again.")
	}
	sess, err := s.IssueSession(ctx, val.Str(row["user_id"]), val.Str(row["tenant_id"]), val.Str(row["device_id"]))
	if e, ok := err.(*httpx.Error); ok && e.Code == httpx.Forbidden {
		// Removed from that class since: fall back to an unscoped session.
		return s.IssueSession(ctx, val.Str(row["user_id"]), "", val.Str(row["device_id"]))
	}
	return sess, err
}

func (s *Service) Revoke(ctx context.Context, token string) error {
	_, err := s.M.C("refresh_tokens").UpdateOne(ctx, bson.M{"_id": sha(token)}, bson.M{"$set": bson.M{"revoked": true, "revoked_at": time.Now()}})
	return err
}
