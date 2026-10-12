// Package safety holds the security-sensitive helpers shared by the importer,
// the service layer, the link checker, and the web handlers: the URL
// normalizer (dedupe key), the text sanitizer and prompt-injection filter, the
// SSRF-safe HTTP client, and the in-memory rate limiter.
package safety

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ErrInvalidURL is returned by NormalizeURL for anything that is not an
// acceptable absolute http(s) URL.
var ErrInvalidURL = errors.New("safety: invalid url")

// maxURLLen is the longest raw URL accepted (POST /items validation).
const maxURLLen = 2048

// trackingParams are query keys dropped from every URL (compared
// case-insensitively). utm_* is handled separately as a prefix.
var trackingParams = map[string]bool{
	"ref":       true,
	"ref_src":   true,
	"fbclid":    true,
	"gclid":     true,
	"mc_cid":    true,
	"mc_eid":    true,
	"ascsubtag": true,
	"linkcode":  true,
	"irclickid": true,
	"aff":       true,
	"affiliate": true,
}

// youTubeIDRe matches a plausible YouTube video id.
var youTubeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NormalizeURL returns the canonical clean form of a URL. The result is what
// gets stored and is also the dedupe key (items.url_norm), so it must be
// deterministic and idempotent: NormalizeURL(NormalizeURL(x)) == NormalizeURL(x).
//
// It accepts only http/https URLs without embedded credentials and at most
// 2048 characters. It lowercases scheme and host, upgrades to https, strips
// "www.", default ports, the fragment, trailing slashes (except the bare root),
// tracking and affiliate params, sorts the remaining params, canonicalizes
// GitHub owner/repo paths, and maps every YouTube video URL form to
// https://youtube.com/watch?v=ID.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLen {
		return "", ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalidURL
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrInvalidURL
	}
	if u.User != nil || u.Opaque != "" {
		return "", ErrInvalidURL
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || strings.ContainsAny(host, " \t\r\n@") {
		return "", ErrInvalidURL
	}
	host = strings.TrimPrefix(host, "www.")
	if host == "" {
		return "", ErrInvalidURL
	}
	port := u.Port()
	if port == "80" || port == "443" {
		port = ""
	}

	if id, ok := youTubeVideoID(host, u); ok {
		return "https://youtube.com/watch?v=" + id, nil
	}

	// Rebuild the host, re-bracketing IPv6 literals.
	hostPort := host
	if strings.Contains(host, ":") {
		hostPort = "[" + host + "]"
	}
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}

	path := u.EscapedPath()
	if host == "github.com" {
		path = normalizeGitHubPath(path)
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}

	query := cleanQuery(u.RawQuery, isAmazonHost(host))

	out := "https://" + hostPort + path
	if query != "" {
		out += "?" + query
	}
	if len(out) > maxURLLen {
		return "", ErrInvalidURL
	}
	return out, nil
}

// cleanQuery drops tracking/affiliate params and returns the remaining params
// re-encoded with keys sorted (url.Values.Encode sorts by key). The Amazon
// "tag" param is only dropped on Amazon hosts because elsewhere it is often
// meaningful (e.g. blog tag filters).
func cleanQuery(rawQuery string, amazon bool) string {
	if rawQuery == "" {
		return ""
	}
	// ParseQuery keeps every well-formed pair even when it reports an error
	// for a malformed one, so the error is deliberately ignored.
	values, _ := url.ParseQuery(rawQuery)
	for key := range values {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || trackingParams[lower] || (amazon && lower == "tag") {
			delete(values, key)
		}
	}
	return values.Encode()
}

// normalizeGitHubPath lowercases the owner and repo segments of a github.com
// path (GitHub treats them case-insensitively) and strips a trailing ".git"
// from the repo segment so clone URLs dedupe against repo pages.
func normalizeGitHubPath(path string) string {
	segs := strings.Split(path, "/")
	// segs[0] is the empty string before the leading slash.
	for i := 1; i < len(segs) && i <= 2; i++ {
		segs[i] = strings.ToLower(segs[i])
	}
	if len(segs) > 2 {
		segs[2] = strings.TrimSuffix(segs[2], ".git")
	}
	return strings.Join(segs, "/")
}

// isAmazonHost reports whether host belongs to an Amazon storefront: its
// registrable part is "amazon.<tld>" or "amazon.co(m).<cc>", e.g. amazon.com,
// smile.amazon.com, amazon.co.uk, amazon.com.au, amazon.de.
func isAmazonHost(host string) bool {
	labels := strings.Split(host, ".")
	for i, label := range labels {
		if label != "amazon" {
			continue
		}
		rest := labels[i+1:]
		switch {
		case len(rest) == 1 && rest[0] != "":
			return true
		case len(rest) == 2 && (rest[0] == "co" || rest[0] == "com") && rest[1] != "":
			return true
		}
	}
	return false
}

// youTubeVideoID extracts the video id from youtu.be/ID, youtube.com/watch?v=ID,
// /shorts/ID, /embed/ID, /live/ID and /v/ID on youtube.com or m.youtube.com.
// host must already be lowercased with "www." removed.
func youTubeVideoID(host string, u *url.URL) (string, bool) {
	var id string
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch host {
	case "youtu.be":
		id = segs[0]
	case "youtube.com", "m.youtube.com":
		switch {
		case len(segs) == 1 && segs[0] == "watch":
			id = u.Query().Get("v")
		case len(segs) >= 2 && (segs[0] == "shorts" || segs[0] == "embed" || segs[0] == "live" || segs[0] == "v"):
			id = segs[1]
		}
	default:
		return "", false
	}
	if !youTubeIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// Domain returns the host of a normalized URL (lowercase, "www." stripped, no
// port). It returns "" when the URL cannot be parsed.
func Domain(normURL string) string {
	u, err := url.Parse(normURL)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return strings.TrimPrefix(host, "www.")
}

// gitHubReservedOwners are first path segments on github.com that are site
// pages rather than users or organizations.
var gitHubReservedOwners = map[string]bool{
	"topics": true, "orgs": true, "sponsors": true, "marketplace": true,
	"features": true, "about": true, "login": true, "logout": true,
	"join": true, "signup": true, "settings": true, "apps": true,
	"collections": true, "trending": true, "explore": true, "search": true,
	"notifications": true, "pulls": true, "issues": true, "new": true,
	"organizations": true, "enterprise": true, "pricing": true,
	"security": true, "site": true, "contact": true, "customer-stories": true,
	"readme": true, "events": true, "codespaces": true, "team": true,
	"users": true, "dashboard": true, "stars": true, "watching": true,
	"blog": true, "home": true, "gist": true, "account": true,
}

// GitHubRepo reports owner/repo when normURL is exactly a github.com repo root
// (https://github.com/owner/repo with no further path and no query). Reserved
// site paths such as /topics/x or /orgs/x are not repos.
func GitHubRepo(normURL string) (owner, repo string, ok bool) {
	u, err := url.Parse(normURL)
	if err != nil || Domain(normURL) != "github.com" || u.RawQuery != "" {
		return "", "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		return "", "", false
	}
	owner, repo = strings.ToLower(segs[0]), strings.TrimSuffix(strings.ToLower(segs[1]), ".git")
	if gitHubReservedOwners[owner] || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

// YouTubeID returns the video id of a normalized https://youtube.com/watch?v=ID URL.
func YouTubeID(normURL string) (string, bool) {
	u, err := url.Parse(normURL)
	if err != nil || Domain(normURL) != "youtube.com" || u.Path != "/watch" {
		return "", false
	}
	id := u.Query().Get("v")
	if !youTubeIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// youTubeChannelRe matches a normalized YouTube channel URL: a handle, a
// channel id, or the legacy /c/ and /user/ forms, with nothing after it.
var youTubeChannelRe = regexp.MustCompile(`^https://youtube\.com/(@[^/?#]+|channel/[^/?#]+|c/[^/?#]+|user/[^/?#]+)$`)

// IsYouTubeChannel reports whether normURL is a YouTube channel's home page.
func IsYouTubeChannel(normURL string) bool { return youTubeChannelRe.MatchString(normURL) }

// shortenerDomains are URL shorteners rejected at submit time because they
// hide the real destination and defeat domain-based rules.
var shortenerDomains = map[string]bool{
	"bit.ly": true, "bitly.com": true, "tinyurl.com": true, "t.co": true,
	"goo.gl": true, "ow.ly": true, "is.gd": true, "v.gd": true,
	"buff.ly": true, "cutt.ly": true, "rebrand.ly": true, "shorturl.at": true,
	"tiny.cc": true, "rb.gy": true, "lnkd.in": true, "s.id": true,
	"t.ly": true, "bl.ink": true, "short.io": true, "tiny.one": true,
	"shorte.st": true, "adf.ly": true, "bc.vc": true, "soo.gd": true,
	"clck.ru": true, "qr.ae": true, "trib.al": true, "dlvr.it": true,
	"po.st": true, "mcaf.ee": true, "su.pr": true, "x.co": true,
	"tr.im": true, "cli.gs": true, "1url.com": true, "urlz.fr": true,
	"shorturl.com": true, "snip.ly": true, "smarturl.it": true,
	"aka.ms": true, "amzn.to": true, "amzn.eu": true,
	"fb.me": true, "ift.tt": true, "j.mp": true,
	"db.tt": true, "git.io": true, "wp.me": true,
	"surl.li": true, "u.to": true, "kutt.it": true, "chilp.it": true,
	"zpr.io": true, "tinu.be": true, "ouo.io": true, "shrtco.de": true,
}

// IsShortener reports whether domain (or a parent of it) is a known URL
// shortener. Input is lowercased and "www." is ignored.
func IsShortener(domain string) bool {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	d = strings.TrimPrefix(d, "www.")
	for d != "" {
		if shortenerDomains[d] {
			return true
		}
		i := strings.IndexByte(d, '.')
		if i < 0 {
			return false
		}
		d = d[i+1:]
	}
	return false
}
