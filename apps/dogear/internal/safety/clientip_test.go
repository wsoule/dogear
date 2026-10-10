package safety

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	if got := ClientIP(r); got != "10.0.0.5" {
		t.Fatalf("socket: %s", got)
	}
	// A client-supplied entry on the left must not win over the proxy's.
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("xff: %s", got)
	}
}
