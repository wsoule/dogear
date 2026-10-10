package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"dogear/internal/service"
)

const appleIssuer = "https://appleid.apple.com"

// Apple implements Sign in with Apple. It differs from the other providers:
// the callback is a cross-site form POST, the client secret is an ES256 JWT
// signed with our key (max 6 months, regenerated on a timer), the name is
// only sent on first authorization, and the ID token must be verified
// against Apple's published keys with iss, aud, exp, and nonce checks.
type Apple struct {
	ClientID, TeamID, KeyID string
	Key                     *ecdsa.PrivateKey
	RedirectURL             string
	HTTP                    *http.Client
	AuthBase, TokenURL      string
	KeysURL                 string

	mu       sync.Mutex
	secret   string
	secretAt time.Time
	keys     map[string]*rsa.PublicKey
	keysAt   time.Time
}

func NewApple(clientID, teamID, keyID, privateKeyPEM, baseURL string) (*Apple, error) {
	block, _ := pem.Decode([]byte(strings.ReplaceAll(privateKeyPEM, `\n`, "\n")))
	if block == nil {
		return nil, errors.New("APPLE_PRIVATE_KEY is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse apple key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("APPLE_PRIVATE_KEY is not an EC key")
	}
	return &Apple{ClientID: clientID, TeamID: teamID, KeyID: keyID, Key: ec,
		RedirectURL: baseURL + "/auth/apple/callback", HTTP: &http.Client{Timeout: 10 * time.Second},
		AuthBase: appleIssuer + "/auth/authorize", TokenURL: appleIssuer + "/auth/token", KeysURL: appleIssuer + "/auth/keys"}, nil
}

func (a *Apple) Name() string { return "apple" }

func (a *Apple) AuthURL(state, nonce string) string {
	q := url.Values{"client_id": {a.ClientID}, "redirect_uri": {a.RedirectURL}, "response_type": {"code"},
		"response_mode": {"form_post"}, "scope": {"name email"}, "state": {state}, "nonce": {nonce}}
	return a.AuthBase + "?" + q.Encode()
}

// clientSecret returns the cached ES256 client-secret JWT, regenerating it
// when older than 30 days (Apple caps the lifetime at 6 months).
func (a *Apple) clientSecret() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.secret != "" && time.Since(a.secretAt) < 30*24*time.Hour {
		return a.secret, nil
	}
	now := time.Now()
	header := map[string]string{"alg": "ES256", "kid": a.KeyID}
	claims := map[string]any{"iss": a.TeamID, "iat": now.Unix(), "exp": now.Add(90 * 24 * time.Hour).Unix(),
		"aud": appleIssuer, "sub": a.ClientID}
	signing := b64json(header) + "." + b64json(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, a.Key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	a.secret = signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	a.secretAt = now
	return a.secret, nil
}

func b64json(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *Apple) Callback(ctx context.Context, r *http.Request, nonce string) (service.ExternalIdentity, error) {
	if err := r.ParseForm(); err != nil {
		return service.ExternalIdentity{}, err
	}
	code := r.PostForm.Get("code")
	if code == "" {
		return service.ExternalIdentity{}, ErrDenied
	}
	secret, err := a.clientSecret()
	if err != nil {
		return service.ExternalIdentity{}, fmt.Errorf("apple secret: %w", err)
	}
	form := url.Values{"client_id": {a.ClientID}, "client_secret": {secret}, "code": {code},
		"grant_type": {"authorization_code"}, "redirect_uri": {a.RedirectURL}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return service.ExternalIdentity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return service.ExternalIdentity{}, fmt.Errorf("apple token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return service.ExternalIdentity{}, fmt.Errorf("apple token: status %d", resp.StatusCode)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tok); err != nil {
		return service.ExternalIdentity{}, fmt.Errorf("apple token decode: %w", err)
	}
	claims, err := a.verifyIDToken(ctx, tok.IDToken, nonce)
	if err != nil {
		return service.ExternalIdentity{}, err
	}
	ext := service.ExternalIdentity{Provider: "apple", ProviderUserID: claims.Sub}
	// Apple sends the name only on the very first authorization.
	if u := r.PostForm.Get("user"); u != "" {
		var info struct {
			Name struct {
				FirstName string `json:"firstName"`
				LastName  string `json:"lastName"`
			} `json:"name"`
		}
		if json.Unmarshal([]byte(u), &info) == nil {
			ext.DisplayName = strings.TrimSpace(info.Name.FirstName + " " + info.Name.LastName)
			ext.SuggestedUsername = info.Name.FirstName
		}
	}
	return ext, nil
}

type appleClaims struct {
	Iss   string `json:"iss"`
	Aud   string `json:"aud"`
	Sub   string `json:"sub"`
	Exp   int64  `json:"exp"`
	Nonce string `json:"nonce"`
}

// verifyIDToken checks the RS256 signature against Apple's JWKS and the
// iss, aud, exp, and nonce claims.
func (a *Apple) verifyIDToken(ctx context.Context, idToken, nonce string) (appleClaims, error) {
	var c appleClaims
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return c, errors.New("apple id token malformed")
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &h); err != nil || h.Alg != "RS256" {
		return c, errors.New("apple id token header")
	}
	key, err := a.publicKey(ctx, h.Kid)
	if err != nil {
		return c, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return c, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return c, errors.New("apple id token signature")
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return c, err
	}
	switch {
	case c.Iss != appleIssuer:
		return c, errors.New("apple id token issuer")
	case c.Aud != a.ClientID:
		return c, errors.New("apple id token audience")
	case time.Now().Unix() > c.Exp:
		return c, errors.New("apple id token expired")
	case nonce == "" || c.Nonce != nonce:
		return c, errors.New("apple id token nonce")
	case c.Sub == "":
		return c, errors.New("apple id token subject")
	}
	return c, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// publicKey returns Apple's signing key by kid, refreshing the JWKS hourly
// or when an unknown kid appears.
func (a *Apple) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	k, ok := a.keys[kid]
	fresh := time.Since(a.keysAt) < time.Hour
	a.mu.Unlock()
	if ok && fresh {
		return k, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.KeysURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apple keys: %w", err)
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("apple keys decode: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, j := range jwks.Keys {
		n, err1 := base64.RawURLEncoding.DecodeString(j.N)
		e, err2 := base64.RawURLEncoding.DecodeString(j.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[j.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	a.mu.Lock()
	a.keys, a.keysAt = keys, time.Now()
	a.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("apple id token: unknown key")
}
