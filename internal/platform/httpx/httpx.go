// Package httpx holds the JSON helpers of the v1 API: the error envelope from
// the contract, gzip request bodies from the app, and the middleware that
// turns a panic into a 500 for that one request.
package httpx

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

type Code string

const (
	BadRequest     Code = "bad_request"
	Unauthorized   Code = "unauthorized"
	TokenExpired   Code = "token_expired"
	Forbidden      Code = "forbidden"
	NotFound       Code = "not_found"
	OTPInvalid     Code = "otp_invalid"
	OTPExpired     Code = "otp_expired"
	OTPRateLimited Code = "otp_rate_limited"
	PlanLimit      Code = "plan_limit"
	Conflict       Code = "conflict"
	ServerError    Code = "server_error"
)

var status = map[Code]int{
	BadRequest: 400, Unauthorized: 401, TokenExpired: 401, Forbidden: 403, NotFound: 404,
	OTPInvalid: 400, OTPExpired: 400, OTPRateLimited: 429, PlanLimit: 402, Conflict: 409, ServerError: 500,
}

// Error is an error whose message is safe to show to the teacher or parent.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func Err(code Code, msg string) *Error { return &Error{Code: code, Message: msg} }

func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Fail writes the error envelope. Errors that are not *Error are logged and
// answered with a generic 500.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		slog.ErrorContext(r.Context(), "api_error", "path", r.URL.Path, "err", err)
		e = Err(ServerError, "Something went wrong. Please try again.")
	}
	JSON(w, status[e.Code], map[string]any{"error": map[string]string{"code": string(e.Code), "message": e.Message}})
}

// Handler adapts a function that returns an error.
type Handler func(w http.ResponseWriter, r *http.Request) error

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		Fail(w, r, err)
	}
}

const maxBody = 4 << 20

// Body reads the raw request body, gunzipping it when the app sent gzip.
func Body(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, Err(BadRequest, "Could not read the request.")
	}
	if len(raw) > maxBody {
		return nil, Err(BadRequest, "The request is too large.")
	}
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, Err(BadRequest, "The request body is not valid gzip.")
		}
		raw, err = io.ReadAll(io.LimitReader(zr, 8*maxBody))
		if err != nil {
			return nil, Err(BadRequest, "The request body is not valid gzip.")
		}
	}
	return raw, nil
}

// Read decodes a JSON object body into a map, keeping numbers exact. An
// empty body is an empty object.
func Read(r *http.Request) (map[string]any, error) {
	raw, err := Body(r)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&out); err != nil || out == nil {
		return nil, Err(BadRequest, "The request body is not valid JSON.")
	}
	return out, nil
}

// ReadForm reads a form post or a JSON body into strings, for the parent
// page, which works without JavaScript.
func ReadForm(r *http.Request) (map[string]string, error) {
	out := map[string]string{}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		m, err := Read(r)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			if v != nil {
				out[k] = toString(v)
			}
		}
		return out, nil
	}
	raw, err := Body(r)
	if err != nil {
		return nil, err
	}
	vals, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, Err(BadRequest, "Could not read the form.")
	}
	for k := range vals {
		out[k] = vals.Get(k)
	}
	return out, nil
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Str returns a trimmed string field cut to max runes, or "".
func Str(v any, max int) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func WantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func Redirect303(w http.ResponseWriter, to string) {
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusSeeOther)
}

// Recover turns a panic into a logged 500 for that one request.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				slog.Error("panic", "path", r.URL.Path, "value", v, "stack", string(debug.Stack()))
				Fail(w, r, Err(ServerError, "Something went wrong. Please try again."))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int)           { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Log writes one structured line per request, with no bodies or tokens.
func Log(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		slog.Info("http", "method", r.Method, "path", redact(r.URL.Path), "status", sw.code, "ms", time.Since(start).Milliseconds())
	})
}

// redact hides link tokens in paths such as /v1/parent/<token>/claims.
func redact(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if len(s) > 40 {
			parts[i] = "…"
		}
	}
	return strings.Join(parts, "/")
}
