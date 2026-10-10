package safety

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	tests := []struct {
		hops int
		want string
	}{
		{0, "10.0.0.5"},    // no trusted proxy: header ignored entirely
		{1, "203.0.113.9"}, // one proxy appended the rightmost entry
		{2, "1.2.3.4"},     // two proxies
		{3, "10.0.0.5"},    // fewer entries than hops: fall back to socket
	}
	for _, tt := range tests {
		if got := ClientIP(r, tt.hops); got != tt.want {
			t.Errorf("hops=%d: got %s want %s", tt.hops, got, tt.want)
		}
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := ClientIP(r, 1); got != "10.0.0.5" {
		t.Errorf("garbage header: %s", got)
	}
}

func TestClientIPCloudflare(t *testing.T) {
	trust := ProxyTrust{Hops: 2, SiteHost: "dogear.fyi"}
	mk := func(host, xff, secret string) *http.Request {
		r := httptest.NewRequest("GET", "https://"+host+"/", nil)
		r.RemoteAddr = "10.0.0.5:1234"
		r.Header.Set("CF-Connecting-IP", "198.51.100.7")
		r.Header.Set("X-Forwarded-For", xff)
		if secret != "" {
			r.Header.Set("X-Origin-Secret", secret)
		}
		return r
	}
	viaCF := "172.70.1.2, 10.1.1.1"
	if got := ClientIPWith(mk("dogear.fyi", viaCF, ""), trust); got != "198.51.100.7" {
		t.Fatalf("own zone via cloudflare: %s", got)
	}
	if got := ClientIPWith(mk("web-production.up.railway.app", viaCF, ""), trust); got != "172.70.1.2" {
		t.Fatalf("another host via cloudflare must not trust the header: %s", got)
	}
	if got := ClientIPWith(mk("dogear.fyi", "203.0.113.9, 10.1.1.1", ""), trust); got != "203.0.113.9" {
		t.Fatalf("non-cloudflare peer: %s", got)
	}
	trust.OriginSecret = "s3cret"
	if got := ClientIPWith(mk("dogear.fyi", viaCF, "wrong"), trust); got != "172.70.1.2" {
		t.Fatalf("wrong secret: %s", got)
	}
	if got := ClientIPWith(mk("dogear.fyi", viaCF, "s3cret"), trust); got != "198.51.100.7" {
		t.Fatalf("right secret: %s", got)
	}
}
