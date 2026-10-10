package safety

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the caller's address for rate limiting. X-Forwarded-For
// is trusted only when trustedHops proxies are known to sit in front of the
// app (TRUSTED_PROXY_HOPS): each proxy appends one entry, so the client is
// the trustedHops-th entry from the right and anything further left is
// client-controlled. With trustedHops = 0 the header is ignored and the
// socket address is used.
func ClientIP(r *http.Request, trustedHops int) string {
	if trustedHops > 0 {
		var parts []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			for _, p := range strings.Split(h, ",") {
				if p = strings.TrimSpace(p); p != "" {
					parts = append(parts, p)
				}
			}
		}
		if i := len(parts) - trustedHops; i >= 0 && i < len(parts) {
			if ip := net.ParseIP(parts[i]); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
