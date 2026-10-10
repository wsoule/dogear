package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignerRoundTrip(t *testing.T) {
	s := Signer{Secret: []byte("0123456789abcdef0123456789abcdef")}
	tok, err := s.Sign("pending", map[string]string{"a": "b"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := s.Verify("pending", tok, &out); err != nil || out["a"] != "b" {
		t.Fatalf("verify: %v %v", err, out)
	}
	if err := s.Verify("other", tok, &out); err == nil {
		t.Fatal("purpose not bound")
	}
	if err := s.Verify("pending", tok[:len(tok)-2]+"xx", &out); err == nil {
		t.Fatal("tamper accepted")
	}
	expired, _ := s.Sign("pending", 1, -time.Second)
	if err := s.Verify("pending", expired, &out); err == nil {
		t.Fatal("expired accepted")
	}
}

func TestCSRF(t *testing.T) {
	s := Signer{Secret: []byte("0123456789abcdef0123456789abcdef")}
	tok := s.CSRFToken("session-a")
	if !s.CheckCSRF("session-a", tok) {
		t.Fatal("valid token rejected")
	}
	if s.CheckCSRF("session-b", tok) || s.CheckCSRF("", "") || s.CheckCSRF("session-a", "") {
		t.Fatal("invalid token accepted")
	}
}

// TestAppleIDToken signs a fake Apple ID token with a test RSA key served
// from a fake JWKS endpoint and checks every claim rule.
func TestAppleIDToken(t *testing.T) {
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "n": base64.RawURLEncoding.EncodeToString(rk.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rk.E)).Bytes()),
		}}})
	}))
	defer jwks.Close()
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a := &Apple{ClientID: "fyi.dogear.web", TeamID: "T", KeyID: "K", Key: ek, HTTP: jwks.Client(), KeysURL: jwks.URL}

	sign := func(claims map[string]any) string {
		signing := b64json(map[string]string{"alg": "RS256", "kid": "k1"}) + "." + b64json(claims)
		sum := sha256.Sum256([]byte(signing))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, rk, crypto.SHA256, sum[:])
		return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	good := map[string]any{"iss": appleIssuer, "aud": "fyi.dogear.web", "sub": "001.abc", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "n1"}
	if c, err := a.verifyIDToken(context.Background(), sign(good), "n1"); err != nil || c.Sub != "001.abc" {
		t.Fatalf("good token: %v", err)
	}
	cases := map[string]func(m map[string]any){
		"issuer":   func(m map[string]any) { m["iss"] = "https://evil" },
		"audience": func(m map[string]any) { m["aud"] = "other" },
		"expired":  func(m map[string]any) { m["exp"] = time.Now().Add(-time.Minute).Unix() },
		"nonce":    func(m map[string]any) { m["nonce"] = "n2" },
	}
	for name, mut := range cases {
		m := map[string]any{}
		for k, v := range good {
			m[k] = v
		}
		mut(m)
		if _, err := a.verifyIDToken(context.Background(), sign(m), "n1"); err == nil {
			t.Errorf("%s: bad token accepted", name)
		}
	}
	if s, err := a.clientSecret(); err != nil || len(s) < 50 {
		t.Fatalf("client secret: %v", err)
	}
}
