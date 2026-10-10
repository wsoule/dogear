package safety

import (
	"crypto/subtle"
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
	return ClientIPWith(r, ProxyTrust{Hops: trustedHops})
}

// ProxyTrust describes the proxies in front of the app.
type ProxyTrust struct {
	Hops int // proxies that append to X-Forwarded-For (2 on Railway)
	// SiteHost is the public host Cloudflare serves (e.g. "dogear.fyi").
	// CF-Connecting-IP is only trusted for requests to this host, so another
	// Cloudflare zone pointed at the origin cannot choose its own address.
	SiteHost string
	// OriginSecret, when set, must arrive in the X-Origin-Secret header,
	// added by a Cloudflare request rule on the site's own zone. It proves
	// the request came through that zone rather than any Cloudflare account.
	OriginSecret string
}

// ClientIPWith returns the visitor's address given the proxy setup.
func ClientIPWith(r *http.Request, t ProxyTrust) string {
	peer := proxiedPeer(r, t.Hops)
	// Behind Cloudflare, the peer the proxies report is a Cloudflare server;
	// the visitor is in CF-Connecting-IP. It is trusted only when the peer
	// is in Cloudflare's ranges, the request is for the site's own host, and
	// (if configured) the zone's secret header matches.
	if ip := net.ParseIP(peer); ip != nil && isCloudflare(ip) && t.trustsCloudflare(r) {
		if cf := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); cf != nil {
			return cf.String()
		}
	}
	return peer
}

func (t ProxyTrust) trustsCloudflare(r *http.Request) bool {
	if t.SiteHost == "" || !strings.EqualFold(stripPort(r.Host), t.SiteHost) {
		return false
	}
	if t.OriginSecret != "" {
		got := r.Header.Get("X-Origin-Secret")
		return subtle.ConstantTimeCompare([]byte(got), []byte(t.OriginSecret)) == 1
	}
	return true
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// proxiedPeer is the address of whoever connected to the trusted proxies:
// the trustedHops-th X-Forwarded-For entry from the right, or the socket
// address when there are no trusted proxies.
func proxiedPeer(r *http.Request, trustedHops int) string {
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

// cloudflareRanges are Cloudflare's published edge ranges
// (https://www.cloudflare.com/ips/, checked 2026-10-09).
var cloudflareRanges = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
		"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17",
		"162.158.0.0/15", "104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32",
		"2a06:98c0::/29", "2c0f:f248::/32",
	} {
		_, n, err := net.ParseCIDR(c)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// isCloudflare reports whether ip belongs to Cloudflare's edge.
func isCloudflare(ip net.IP) bool {
	for _, n := range cloudflareRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
