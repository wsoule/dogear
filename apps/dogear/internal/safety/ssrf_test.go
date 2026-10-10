package safety

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		// IPv4 blocked
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"255.255.255.255", true},
		{"240.0.0.1", true},
		{"192.0.2.1", true},
		{"198.18.0.1", true},
		// IPv4 allowed
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
		{"172.15.255.255", false},
		{"172.32.0.0", false},
		{"100.63.255.255", false},
		{"100.128.0.0", false},
		{"169.253.255.255", false},
		{"11.0.0.1", false},
		// IPv6 blocked
		{"::1", true},
		{"::", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		{"fd00:ec2::254", true},
		{"fe80::1", true},
		{"febf::1", true},
		{"ff02::1", true},
		{"2001:db8::1", true},
		// IPv4-mapped / embedded forms of blocked v4
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:10.0.0.1", true},
		{"::ffff:100.64.0.1", true},
		{"::127.0.0.1", true},
		{"64:ff9b::7f00:1", true},
		{"64:ff9b::a9fe:a9fe", true},
		{"2002:7f00:1::", true},
		{"2002:a9fe:a9fe::1", true},
		// IPv6 allowed
		{"2606:4700:4700::1111", false},
		{"2001:4860:4860::8888", false},
		{"::ffff:8.8.8.8", false},
		{"64:ff9b::808:808", false},
		{"2002:808:808::1", false},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", tt.ip)
		}
		if got := IsBlockedIP(ip); got != tt.blocked {
			t.Errorf("IsBlockedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
		}
		// The 4-byte form must agree with the 16-byte form.
		if v4 := ip.To4(); v4 != nil {
			if got := IsBlockedIP(v4); got != tt.blocked {
				t.Errorf("IsBlockedIP(%s as 4 bytes) = %v, want %v", tt.ip, got, tt.blocked)
			}
		}
	}
	if !IsBlockedIP(nil) {
		t.Error("IsBlockedIP(nil) should be true")
	}
	if !IsBlockedIP(net.IP{1, 2, 3}) {
		t.Error("IsBlockedIP(malformed) should be true")
	}
}

// loopbackOnlyPolicy allows exactly the given httptest server address (any
// port) while every other address goes through the real block list. It lets
// tests run a local "public" server and check that redirects from it to
// internal addresses are still refused.
func loopbackOnlyPolicy(t *testing.T, srv *httptest.Server) policy {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	allowedAddr := netip.MustParseAddr(u.Hostname())
	allowedPort, _ := strconv.Atoi(u.Port())
	return policy{
		addrAllowed: func(a netip.Addr) bool { return a.Unmap() == allowedAddr || !isBlockedAddr(a) },
		portAllowed: func(p int) bool { return p == 80 || p == 443 || p == allowedPort },
	}
}

func TestClientRefusesLoopbackServer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	// Default client: blocked (the random port alone is already disallowed).
	_, err := NewClient(ClientOptions{Timeout: 2 * time.Second}).Get(srv.URL)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("default client: err = %v, want ErrBlockedAddress", err)
	}

	// Even with any port allowed, the loopback IP itself is refused.
	anyPort := policy{addrAllowed: defaultPolicy.addrAllowed, portAllowed: func(int) bool { return true }}
	_, err = newClientWithPolicy(ClientOptions{Timeout: 2 * time.Second}, anyPort).Get(srv.URL)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("ip check: err = %v, want ErrBlockedAddress", err)
	}

	// localhost resolves to loopback and must be refused after DNS.
	localhostURL := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	_, err = newClientWithPolicy(ClientOptions{Timeout: 2 * time.Second}, anyPort).Get(localhostURL)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("localhost: err = %v, want ErrBlockedAddress", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached %d times", hits.Load())
	}

	// Sanity check: the test-only AllowPrivate client does reach it.
	resp, err := NewClient(ClientOptions{Timeout: 2 * time.Second, AllowPrivate: true}).Get(srv.URL)
	if err != nil {
		t.Fatalf("AllowPrivate client: %v", err)
	}
	resp.Body.Close()
}

func TestClientRefusesRedirectToPrivate(t *testing.T) {
	targets := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"http://192.168.0.1/admin",
		"http://[::1]/",
		"http://[fd00:ec2::254]/",
		"http://[::ffff:127.0.0.2]/",
		"http://0.0.0.0/",
		"http://127.0.0.2/",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target, http.StatusFound)
			}))
			defer srv.Close()
			c := newClientWithPolicy(ClientOptions{Timeout: 2 * time.Second, MaxRedirects: 5}, loopbackOnlyPolicy(t, srv))
			resp, err := c.Get(srv.URL)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("redirect to %s was followed", target)
			}
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("err = %v, want ErrBlockedAddress", err)
			}
		})
	}
}

func TestClientRejectsPorts(t *testing.T) {
	c := NewClient(ClientOptions{Timeout: 2 * time.Second})
	for _, raw := range []string{
		"http://example.com:8080/",
		"https://example.com:8443/",
		"http://93.184.216.34:22/",
		"http://example.com:0/",
	} {
		_, err := c.Get(raw)
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("Get(%s): err = %v, want ErrBlockedAddress", raw, err)
		}
	}

	// Redirect to a non-standard port is refused by CheckRedirect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com:8080/", http.StatusFound)
	}))
	defer srv.Close()
	rc := newClientWithPolicy(ClientOptions{Timeout: 2 * time.Second, MaxRedirects: 5}, loopbackOnlyPolicy(t, srv))
	if _, err := rc.Get(srv.URL); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("redirect to :8080: err = %v, want ErrBlockedAddress", err)
	}
}

func TestClientRejectsSchemes(t *testing.T) {
	c := NewClient(ClientOptions{Timeout: 2 * time.Second})
	if _, err := c.Get("ftp://example.com/file"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("ftp: err = %v, want ErrBlockedAddress", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer srv.Close()
	rc := newClientWithPolicy(ClientOptions{Timeout: 2 * time.Second, MaxRedirects: 5}, loopbackOnlyPolicy(t, srv))
	if _, err := rc.Get(srv.URL); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("redirect to file://: err = %v, want ErrBlockedAddress", err)
	}
}

func TestDialContextChecks(t *testing.T) {
	dial := defaultPolicy.dialContext(&net.Dialer{Timeout: time.Second, Control: defaultPolicy.controlFunc})
	ctx := context.Background()
	for _, addr := range []string{
		"127.0.0.1:80",
		"169.254.169.254:80",
		"[::1]:443",
		"[fd00:ec2::254]:80",
		"localhost:443",
		"93.184.216.34:8080",
	} {
		conn, err := dial(ctx, "tcp", addr)
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("dial(%s): err = %v, want ErrBlockedAddress", addr, err)
		}
	}

	for _, addr := range []string{"127.0.0.1:80", "10.1.2.3:443", "[::1]:80", "8.8.8.8:8080"} {
		if err := defaultPolicy.controlFunc("tcp", addr, nil); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("controlFunc(%s): err = %v, want ErrBlockedAddress", addr, err)
		}
	}
	if err := defaultPolicy.controlFunc("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("controlFunc(8.8.8.8:443): %v", err)
	}
}

func TestClientMaxRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n > 0 {
			http.Redirect(w, r, srv.URL+"/?n="+strconv.Itoa(n-1), http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer srv.Close()

	c := NewClient(ClientOptions{Timeout: 2 * time.Second, MaxRedirects: 3, AllowPrivate: true})
	resp, err := c.Get(srv.URL + "/?n=3")
	if err != nil {
		t.Fatalf("3 redirects with MaxRedirects=3: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "done" {
		t.Fatalf("body = %q", body)
	}

	if _, err := c.Get(srv.URL + "/?n=4"); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("4 redirects with MaxRedirects=3: err = %v", err)
	}

	none := NewClient(ClientOptions{Timeout: 2 * time.Second, MaxRedirects: 0, AllowPrivate: true})
	if _, err := none.Get(srv.URL + "/?n=1"); err == nil {
		t.Fatal("MaxRedirects=0 followed a redirect")
	}
}

func TestClientUserAgent(t *testing.T) {
	var agents []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agents = append(agents, r.UserAgent())
		if r.URL.Path == "/start" {
			http.Redirect(w, r, srv.URL+"/end", http.StatusFound)
		}
	}))
	defer srv.Close()

	const ua = "AwesomeFeedBot/1.0 (+https://example.com/about)"
	c := NewClient(ClientOptions{Timeout: 2 * time.Second, MaxRedirects: 2, UserAgent: ua, AllowPrivate: true})
	resp, err := c.Get(srv.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(agents) != 2 || agents[0] != ua || agents[1] != ua {
		t.Fatalf("user agents = %q, want %q on both hops", agents, ua)
	}

	agents = nil
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/end", nil)
	req.Header.Set("User-Agent", "custom/2")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(agents) != 1 || agents[0] != "custom/2" {
		t.Fatalf("explicit user agent overridden: %q", agents)
	}
}

func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	c := NewClient(ClientOptions{Timeout: 100 * time.Millisecond, AllowPrivate: true})
	start := time.Now()
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
}

func TestClientIgnoresProxyEnv(t *testing.T) {
	c := NewClient(ClientOptions{})
	gt, ok := c.Transport.(*guardTransport)
	if !ok {
		t.Fatalf("transport is %T", c.Transport)
	}
	if tr, ok := gt.base.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("transport must not use a proxy")
	}
}

func TestReadCapped(t *testing.T) {
	tests := []struct {
		in   string
		n    int64
		want string
	}{
		{"hello world", 5, "hello"},
		{"hello", 5, "hello"},
		{"hi", 5, "hi"},
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"", 10, ""},
	}
	for _, tt := range tests {
		got, err := ReadCapped(strings.NewReader(tt.in), tt.n)
		if err != nil {
			t.Fatalf("ReadCapped(%q, %d): %v", tt.in, tt.n, err)
		}
		if string(got) != tt.want {
			t.Errorf("ReadCapped(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
	big := strings.NewReader(strings.Repeat("x", 1<<20))
	got, err := ReadCapped(big, 512*1024)
	if err != nil || len(got) != 512*1024 {
		t.Fatalf("ReadCapped big: len %d, err %v", len(got), err)
	}
}
