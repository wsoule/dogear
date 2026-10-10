package safety

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the caller's address for rate limiting. Behind Railway's
// proxy the client address is the rightmost X-Forwarded-For entry (the one
// the proxy appended); leftmost entries are client-controlled and spoofable.
// Without the header it falls back to the socket address.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := strings.TrimSpace(parts[i]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
