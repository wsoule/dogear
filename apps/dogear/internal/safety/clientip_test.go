package safety

import (
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
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("CF-Connecting-IP", "198.51.100.7")
	// Peer is a Cloudflare edge address: trust CF-Connecting-IP.
	r.Header.Set("X-Forwarded-For", "172.70.1.2, 10.1.1.1")
	if got := ClientIP(r, 2); got != "198.51.100.7" {
		t.Fatalf("via cloudflare: %s", got)
	}
	// Peer is not Cloudflare (direct to origin): the header is ignored.
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.1.1.1")
	if got := ClientIP(r, 2); got != "203.0.113.9" {
		t.Fatalf("direct with forged header: %s", got)
	}
}
