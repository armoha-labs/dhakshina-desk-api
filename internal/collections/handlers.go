package collections

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/armohalabs/dhakshina-desk-api/internal/platform/httpx"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/store"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/val"
)

func (s *Service) Routes(r chi.Router) {
	r.Method("POST", "/links/parent", httpx.Handler(s.parentLink))
	r.Method("GET", "/parent/{token}", httpx.Handler(s.family))
	r.Method("POST", "/parent/{token}/claims", httpx.Handler(s.claim))
	r.Method("POST", "/parent/{token}/consent", httpx.Handler(s.consent))
	r.Method("POST", "/parent/{token}/requests", httpx.Handler(s.request))
}

var expired = httpx.Err(httpx.NotFound, "This link has expired. Ask your teacher for a new one.")

func (s *Service) who(r *http.Request) (string, string, error) {
	tid, gid, ok := s.Links.VerifyParent(chi.URLParam(r, "token"))
	if !ok {
		return "", "", expired
	}
	return tid, gid, nil
}

// parentLink is the family page link for a guardian, for "Share on WhatsApp"
// in the app. Signed and expiring; no login for the parent.
func (s *Service) parentLink(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Teacher(r)
	if err != nil {
		return err
	}
	b, err := httpx.Read(r)
	if err != nil {
		return err
	}
	gid := httpx.Str(b["guardian_id"], 64)
	if gid == "" {
		return httpx.Err(httpx.BadRequest, "Which guardian?")
	}
	g, err := store.For(s.M.DB, p.TenantID).FindOne(r.Context(), "guardians", bson.M{"_id": gid, "deleted": bson.M{"$ne": true}})
	if err != nil {
		return err
	}
	if g == nil {
		return httpx.Err(httpx.NotFound, "That guardian has not synced yet.")
	}
	tok, exp := s.Links.Parent(p.TenantID, gid)
	httpx.JSON(w, 200, bson.M{"url": s.WebURL("/p/" + tok), "expires_at": val.ISO(exp)})
	return nil
}

func (s *Service) family(w http.ResponseWriter, r *http.Request) error {
	tid, gid, err := s.who(r)
	if err != nil {
		return err
	}
	f, err := s.LoadFamily(r.Context(), tid, gid)
	if err != nil {
		return err
	}
	if f == nil {
		return expired
	}
	httpx.JSON(w, 200, f)
	return nil
}

// The parent page works as plain HTML forms (no JavaScript) and as JSON from
// the website's server. Form posts get a 303 back to the page.
func (s *Service) back(w http.ResponseWriter, r *http.Request, suffix string) {
	httpx.Redirect303(w, s.WebURL("/p/"+url.PathEscape(chi.URLParam(r, "token"))+suffix))
}

func (s *Service) claim(w http.ResponseWriter, r *http.Request) error {
	tid, gid, err := s.who(r)
	if err != nil {
		return err
	}
	f, err := httpx.ReadForm(r)
	if err != nil {
		return err
	}
	eid := f["enrolment_id"]
	id, dup, err := s.CreateClaim(r.Context(), tid, gid, eid, f["utr"], f["app"])
	form := !httpx.WantsJSON(r) && !strings.Contains(r.Header.Get("Content-Type"), "application/json")
	if err != nil {
		if he, ok := err.(*httpx.Error); ok && form {
			s.back(w, r, "?err="+url.QueryEscape(he.Message)+"&open="+url.QueryEscape(eid)+"#e-"+eid)
			return nil
		}
		return err
	}
	if form {
		s.back(w, r, "?sent="+url.QueryEscape(eid)+"#e-"+eid)
		return nil
	}
	var idV any
	if id != "" {
		idV = id
	}
	httpx.JSON(w, 201, bson.M{"id": idV, "duplicate": dup})
	return nil
}

func clientIP(r *http.Request, f map[string]string) string {
	if ip := strings.TrimSpace(f["ip"]); ip != "" {
		return ip
	}
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		return strings.TrimSpace(strings.Split(xf, ",")[0])
	}
	return ""
}

func (s *Service) consent(w http.ResponseWriter, r *http.Request) error {
	tid, gid, err := s.who(r)
	if err != nil {
		return err
	}
	f, err := httpx.ReadForm(r)
	if err != nil {
		return err
	}
	choice := "given"
	if f["choice"] == "withdrawn" {
		choice = "withdrawn"
	}
	if err := s.SetConsent(r.Context(), tid, gid, choice, "web", clientIP(r, f)); err != nil {
		return err
	}
	if !httpx.WantsJSON(r) && !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if choice == "given" {
			s.back(w, r, "?consent=1")
		} else {
			s.back(w, r, "/privacy?withdrawn=1")
		}
		return nil
	}
	httpx.NoContent(w)
	return nil
}

func (s *Service) request(w http.ResponseWriter, r *http.Request) error {
	tid, gid, err := s.who(r)
	if err != nil {
		return err
	}
	f, err := httpx.ReadForm(r)
	if err != nil {
		return err
	}
	kind := "access"
	if f["kind"] == "erase" || f["kind"] == "correct" {
		kind = f["kind"]
	}
	if err := s.CreateDataRequest(r.Context(), tid, gid, kind, f["note"]); err != nil {
		return err
	}
	if !httpx.WantsJSON(r) && !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		s.back(w, r, "/privacy?requested="+kind)
		return nil
	}
	httpx.NoContent(w)
	return nil
}
