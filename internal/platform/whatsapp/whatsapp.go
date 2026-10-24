// Package whatsapp sends messages through Meta's WhatsApp Cloud API from the
// one shared app number. Without credentials (development) it logs instead.
package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	Token, PhoneID, AppSecret, APIBase string
	HTTP                               *http.Client
}

func New(token, phoneID, appSecret, apiBase string) *Client {
	return &Client{Token: token, PhoneID: phoneID, AppSecret: appSecret, APIBase: strings.TrimRight(apiBase, "/"),
		HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) Live() bool { return c.Token != "" && c.PhoneID != "" }

// Template is a platform-owned, Meta-approved utility template.
type Template struct {
	Name   string
	Lang   string
	Params []string
	// ButtonURLSuffix fills the dynamic part of a URL button (index 0).
	ButtonURLSuffix string
	// QuickReplies sets the payload of quick-reply buttons, in order, after
	// any URL button.
	QuickReplies []string
}

// Error is a send failure; Permanent means retrying will not help.
type Error struct {
	Status    int
	Message   string
	Permanent bool
}

func (e *Error) Error() string { return fmt.Sprintf("whatsapp %d: %s", e.Status, e.Message) }

// SendTemplate sends a template message and returns Meta's message id.
func (c *Client) SendTemplate(ctx context.Context, toE164 string, t Template) (string, error) {
	params := make([]map[string]string, len(t.Params))
	for i, p := range t.Params {
		params[i] = map[string]string{"type": "text", "text": p}
	}
	components := []any{map[string]any{"type": "body", "parameters": params}}
	idx := 0
	if t.ButtonURLSuffix != "" {
		components = append(components, map[string]any{"type": "button", "sub_type": "url", "index": fmt.Sprint(idx),
			"parameters": []map[string]string{{"type": "text", "text": t.ButtonURLSuffix}}})
		idx++
	}
	for _, p := range t.QuickReplies {
		components = append(components, map[string]any{"type": "button", "sub_type": "quick_reply", "index": fmt.Sprint(idx),
			"parameters": []map[string]string{{"type": "payload", "payload": p}}})
		idx++
	}
	lang := t.Lang
	if lang == "" {
		lang = "en"
	}
	return c.send(ctx, toE164, t.Name, map[string]any{
		"type":     "template",
		"template": map[string]any{"name": t.Name, "language": map[string]string{"code": lang}, "components": components},
	})
}

// SendText sends a free-form reply, allowed only inside the 24-hour window
// after the parent wrote to the number.
func (c *Client) SendText(ctx context.Context, toE164, body string) (string, error) {
	return c.send(ctx, toE164, "text", map[string]any{"type": "text", "text": map[string]any{"body": body, "preview_url": true}})
}

func (c *Client) send(ctx context.Context, to, label string, msg map[string]any) (string, error) {
	if !c.Live() {
		slog.Info("whatsapp_dev_send", "to", mask(to), "what", label)
		return "dev." + fmt.Sprint(time.Now().UnixNano()), nil
	}
	msg["messaging_product"] = "whatsapp"
	msg["to"] = strings.TrimPrefix(to, "+")
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+"/"+c.PhoneID+"/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", &Error{Message: err.Error()}
	}
	defer res.Body.Close()
	var out struct {
		Messages []struct{ ID string } `json:"messages"`
		Error    *struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode >= 300 || len(out.Messages) == 0 {
		e := &Error{Status: res.StatusCode, Message: fmt.Sprintf("HTTP %d", res.StatusCode)}
		if out.Error != nil {
			e.Message = out.Error.Message
		}
		// 4xx other than rate limits will fail the same way again.
		e.Permanent = res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 429
		return "", e
	}
	return out.Messages[0].ID, nil
}

// VerifySignature checks X-Hub-Signature-256 over the raw webhook body.
func (c *Client) VerifySignature(raw []byte, header string) bool {
	if c.AppSecret == "" {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(c.AppSecret))
	m.Write(raw)
	return hmac.Equal(got, m.Sum(nil))
}

func mask(p string) string {
	if len(p) > 5 {
		return p[:5] + "…"
	}
	return "…"
}
