// Package auth implements the OAuth sign-in providers (GitHub, Google,
// Apple), signed cookies, CSRF tokens, and Cloudflare Turnstile checks.
// Session and token storage live in the service package.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dogear/internal/service"
)

// Provider is one OAuth sign-in method.
type Provider interface {
	Name() string
	// AuthURL is where to send the browser to start sign-in.
	AuthURL(state, nonce string) string
	// Callback validates the provider's redirect and returns the identity.
	Callback(ctx context.Context, r *http.Request, nonce string) (service.ExternalIdentity, error)
}

var ErrDenied = errors.New("sign-in was cancelled or denied")

// Signer signs and verifies small cookie payloads with HMAC-SHA256.
type Signer struct{ Secret []byte }

func (s Signer) mac(purpose string, data []byte) []byte {
	m := hmac.New(sha256.New, s.Secret)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write(data)
	return m.Sum(nil)
}

// Sign encodes v as JSON with an expiry and an HMAC tag bound to purpose.
func (s Signer) Sign(purpose string, v any, ttl time.Duration) (string, error) {
	body, err := json.Marshal(struct {
		V   any   `json:"v"`
		Exp int64 `json:"exp"`
	}{v, time.Now().Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(s.mac(purpose, body)), nil
}

// Verify checks the tag and expiry and decodes the payload into v.
func (s Signer) Verify(purpose, token string, v any) error {
	b64, tag, ok := strings.Cut(token, ".")
	if !ok {
		return errors.New("malformed")
	}
	body, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	got, err := base64.RawURLEncoding.DecodeString(tag)
	if err != nil {
		return err
	}
	if !hmac.Equal(got, s.mac(purpose, body)) {
		return errors.New("bad signature")
	}
	var env struct {
		V   json.RawMessage `json:"v"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return err
	}
	if time.Now().Unix() > env.Exp {
		return errors.New("expired")
	}
	return json.Unmarshal(env.V, v)
}

// CSRFToken derives the per-session CSRF token from the session cookie value.
func (s Signer) CSRFToken(sessionRaw string) string {
	return base64.RawURLEncoding.EncodeToString(s.mac("csrf", []byte(sessionRaw)))
}

// CheckCSRF compares a submitted token with the session's expected token.
func (s Signer) CheckCSRF(sessionRaw, token string) bool {
	if sessionRaw == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s.CSRFToken(sessionRaw)), []byte(token)) == 1
}

// Turnstile verifies Cloudflare Turnstile responses. With no secret
// configured (local development) every check passes.
type Turnstile struct {
	SiteKey, Secret string
	HTTP            *http.Client
}

func (t Turnstile) Enabled() bool { return t.Secret != "" }

func (t Turnstile) Verify(ctx context.Context, response, remoteIP string) bool {
	if !t.Enabled() {
		return true
	}
	if response == "" {
		return false
	}
	form := url.Values{"secret": {t.Secret}, "response": {response}, "remoteip": {remoteIP}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return false
	}
	return out.Success
}

func getJSON(ctx context.Context, client *http.Client, u, bearer string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "DogearBot/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}
