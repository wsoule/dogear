package safety

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned (wrapped) when a request targets a disallowed
// scheme, port, or IP address, either directly or through a redirect.
var ErrBlockedAddress = errors.New("safety: blocked address")

// ClientOptions configures NewClient.
type ClientOptions struct {
	// Timeout caps the whole exchange including redirects and body reads.
	// Zero means 10 seconds.
	Timeout time.Duration
	// MaxRedirects is the number of redirects followed before giving up.
	// Zero means redirects are refused.
	MaxRedirects int
	// UserAgent is set on every outgoing request that has no User-Agent.
	UserAgent string
	// AllowPrivate disables the IP and port checks. For tests only (it lets a
	// client reach an httptest server on 127.0.0.1:random-port).
	AllowPrivate bool
}

// blockedPrefixes are the IP ranges no outbound fetch may reach: loopback,
// private, link-local (incl. cloud metadata), CGNAT, unspecified, multicast,
// documentation/benchmark/reserved ranges.
var blockedPrefixes = func() []netip.Prefix {
	cidrs := []string{
		// IPv4
		"0.0.0.0/8",       // "this network", includes 0.0.0.0
		"10.0.0.0/8",      // RFC1918
		"100.64.0.0/10",   // CGNAT
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, includes 169.254.169.254 metadata
		"172.16.0.0/12",   // RFC1918
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"192.88.99.0/24",  // 6to4 relay anycast
		"192.168.0.0/16",  // RFC1918
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, includes 255.255.255.255
		// IPv6
		"::/128",        // unspecified
		"::1/128",       // loopback
		"100::/64",      // discard-only
		"2001:db8::/32", // documentation
		"fc00::/7",      // unique local, includes fd00:ec2::254 (AWS metadata)
		"fe80::/10",     // link-local
		"fec0::/10",     // deprecated site-local
		"ff00::/8",      // multicast
	}
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

// Prefixes of IPv6 ranges that embed an IPv4 address; the embedded address is
// checked against the IPv4 rules so e.g. 64:ff9b::7f00:1 cannot reach 127.0.0.1.
var (
	nat64Prefix      = netip.MustParsePrefix("64:ff9b::/96")
	nat64LocalPrefix = netip.MustParsePrefix("64:ff9b:1::/48")
	v4CompatPrefix   = netip.MustParsePrefix("::/96")
	sixToFourPrefix  = netip.MustParsePrefix("2002::/16")
)

// IsBlockedIP reports whether ip must never be contacted by the fetcher.
// IPv4-mapped (::ffff:a.b.c.d), IPv4-compatible, NAT64 and 6to4 IPv6 forms
// are unwrapped and checked as IPv4. A nil or malformed IP is blocked.
func IsBlockedIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return isBlockedAddr(addr)
}

// isBlockedAddr is IsBlockedIP for netip.Addr.
func isBlockedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		raw := addr.As16()
		switch {
		case nat64Prefix.Contains(addr), nat64LocalPrefix.Contains(addr):
			return isBlockedAddr(netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}))
		case sixToFourPrefix.Contains(addr):
			return isBlockedAddr(netip.AddrFrom4([4]byte{raw[2], raw[3], raw[4], raw[5]}))
		case v4CompatPrefix.Contains(addr) && addr != netip.IPv6Unspecified() && addr != netip.IPv6Loopback():
			return isBlockedAddr(netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}))
		}
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// policy decides which destinations a client may reach. Production uses
// defaultPolicy; tests can swap in a narrower exception (e.g. one loopback
// address) while keeping every other rule active.
type policy struct {
	addrAllowed func(netip.Addr) bool
	portAllowed func(int) bool
}

var defaultPolicy = policy{
	addrAllowed: func(a netip.Addr) bool { return !isBlockedAddr(a) },
	portAllowed: func(p int) bool { return p == 80 || p == 443 },
}

var permissivePolicy = policy{
	addrAllowed: func(netip.Addr) bool { return true },
	portAllowed: func(int) bool { return true },
}

// checkRequest enforces the scheme and port rules on a request URL before
// any connection is attempted.
func (p policy) checkRequest(req *http.Request) error {
	u := req.URL
	if u == nil {
		return fmt.Errorf("%w: missing url", ErrBlockedAddress)
	}
	var port int
	switch u.Scheme {
	case "http":
		port = 80
	case "https":
		port = 443
	default:
		return fmt.Errorf("%w: scheme %q not allowed", ErrBlockedAddress, u.Scheme)
	}
	if ps := u.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil {
			return fmt.Errorf("%w: bad port %q", ErrBlockedAddress, ps)
		}
		port = n
	}
	if !p.portAllowed(port) {
		return fmt.Errorf("%w: port %d not allowed", ErrBlockedAddress, port)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: missing host", ErrBlockedAddress)
	}
	return nil
}

// dialContext resolves the host itself, drops every disallowed address, and
// dials the checked IP directly so a second DNS lookup (DNS rebinding /
// TOCTOU) cannot swap in an internal address. The dialer's Control hook
// re-checks the final socket address as defense in depth.
func (p policy) dialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || !p.portAllowed(port) {
			return nil, fmt.Errorf("%w: port %s not allowed", ErrBlockedAddress, portStr)
		}

		var candidates []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			candidates = []netip.Addr{ip}
		} else {
			candidates, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
		}

		var allowed []netip.Addr
		for _, ip := range candidates {
			if p.addrAllowed(ip) {
				allowed = append(allowed, ip)
			}
		}
		if len(allowed) == 0 {
			return nil, fmt.Errorf("%w: %s resolves only to disallowed addresses", ErrBlockedAddress, host)
		}

		var lastErr error
		for _, ip := range allowed {
			conn, err := dialer.DialContext(ctx, network, netip.AddrPortFrom(ip, uint16(port)).String())
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, lastErr
	}
}

// controlFunc is installed as net.Dialer.Control: it runs just before the
// socket connects and refuses any address the policy disallows.
func (p policy) controlFunc(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparseable dial address %q", ErrBlockedAddress, address)
	}
	if !p.addrAllowed(ap.Addr()) || !p.portAllowed(int(ap.Port())) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
	}
	return nil
}

// guardTransport wraps the real transport to enforce scheme/port rules and
// the default User-Agent on every request, including each redirect hop.
type guardTransport struct {
	base      http.RoundTripper
	policy    policy
	userAgent string
}

func (t *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.policy.checkRequest(req); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if t.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.userAgent)
	}
	return t.base.RoundTrip(req)
}

// NewClient returns an *http.Client for fetching untrusted URLs (link checker,
// submit preview). It allows only http/https on ports 80/443, resolves DNS
// itself and refuses internal addresses (see IsBlockedIP), dials the checked
// IP, re-applies every rule on each redirect, caps redirects at
// opts.MaxRedirects, ignores proxy environment variables, and bounds the
// whole exchange by opts.Timeout. Pair it with ReadCapped to cap body size.
func NewClient(opts ClientOptions) *http.Client {
	p := defaultPolicy
	if opts.AllowPrivate {
		p = permissivePolicy
	}
	return newClientWithPolicy(opts, p)
}

// newClientWithPolicy builds the client for a given policy; tests use it to
// allow a single httptest address while every other rule stays enforced.
func newClientWithPolicy(opts ClientOptions, p policy) *http.Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control:   p.controlFunc,
	}
	base := &http.Transport{
		Proxy:                 nil, // never route through env proxies; they would bypass the IP checks
		DialContext:           p.dialContext(dialer),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}
	maxRedirects := max(opts.MaxRedirects, 0)
	return &http.Client{
		Timeout:   timeout,
		Transport: &guardTransport{base: base, policy: p, userAgent: opts.UserAgent},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("safety: stopped after %d redirects", maxRedirects)
			}
			return p.checkRequest(req)
		},
	}
}

// ReadCapped reads at most n bytes from r. Hitting the cap is not an error:
// callers get the first n bytes, which is enough for title extraction and
// link checks without letting a huge body exhaust memory.
func ReadCapped(r io.Reader, n int64) ([]byte, error) {
	if n <= 0 {
		return []byte{}, nil
	}
	return io.ReadAll(io.LimitReader(r, n))
}
