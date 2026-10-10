package safety

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// scheme / host basics
		{"already clean", "https://example.com/path", "https://example.com/path"},
		{"http upgraded", "http://example.com/a", "https://example.com/a"},
		{"uppercase scheme", "HTTPS://example.com/a", "https://example.com/a"},
		{"uppercase host", "https://EXAMPLE.Com/Path", "https://example.com/Path"},
		{"path case preserved", "https://example.com/CamelCase/Path", "https://example.com/CamelCase/Path"},
		{"strip www", "https://www.example.com/a", "https://example.com/a"},
		{"strip WWW uppercase", "https://WWW.Example.com/a", "https://example.com/a"},
		{"keep other subdomain", "https://docs.example.com/a", "https://docs.example.com/a"},
		{"surrounding whitespace", "  https://example.com/a \n", "https://example.com/a"},
		{"trailing dot host", "https://example.com./a", "https://example.com/a"},

		// ports
		{"strip :443", "https://example.com:443/a", "https://example.com/a"},
		{"strip :80", "http://example.com:80/a", "https://example.com/a"},
		{"keep custom port", "https://example.com:8443/a", "https://example.com:8443/a"},
		{"ipv6 literal", "http://[2606:4700::1]/a", "https://[2606:4700::1]/a"},
		{"ipv6 literal with port", "http://[2606:4700::1]:8080/a", "https://[2606:4700::1]:8080/a"},

		// fragments and slashes
		{"strip fragment", "https://example.com/a#section", "https://example.com/a"},
		{"strip trailing slash", "https://example.com/a/", "https://example.com/a"},
		{"strip multiple trailing slashes", "https://example.com/a///", "https://example.com/a"},
		{"bare root keeps slash", "https://example.com/", "https://example.com/"},
		{"bare host gets slash", "https://example.com", "https://example.com/"},
		{"root with fragment", "https://example.com/#top", "https://example.com/"},
		{"root with query", "https://example.com/?b=2&a=1", "https://example.com/?a=1&b=2"},
		{"empty query dropped", "https://example.com/a?", "https://example.com/a"},
		{"encoded path kept", "https://example.com/a%20b/c%2Fd", "https://example.com/a%20b/c%2Fd"},

		// tracking params
		{"utm params", "https://example.com/a?utm_source=x&utm_medium=y&utm_campaign=z", "https://example.com/a"},
		{"utm uppercase key", "https://example.com/a?UTM_Source=x&id=1", "https://example.com/a?id=1"},
		{"ref and ref_src", "https://example.com/a?ref=hn&ref_src=twsrc&page=2", "https://example.com/a?page=2"},
		{"fbclid gclid", "https://example.com/?fbclid=abc&gclid=def", "https://example.com/"},
		{"mailchimp", "https://example.com/a?mc_cid=1&mc_eid=2&x=y", "https://example.com/a?x=y"},
		{"sort params", "https://example.com/s?z=1&a=2&m=3", "https://example.com/s?a=2&m=3&z=1"},
		{"keep repeated params in order", "https://example.com/s?b=2&a=1&a=0", "https://example.com/s?a=1&a=0&b=2"},
		{"referrer not dropped", "https://example.com/s?referrer=1", "https://example.com/s?referrer=1"},

		// affiliate params
		{"amazon tag", "https://www.amazon.com/dp/B000?tag=foo-20&th=1", "https://amazon.com/dp/B000?th=1"},
		{"amazon co.uk tag", "https://amazon.co.uk/dp/B000?tag=foo-21", "https://amazon.co.uk/dp/B000"},
		{"amazon de tag", "https://www.amazon.de/dp/B000?tag=foo-21", "https://amazon.de/dp/B000"},
		{"amazon com.au tag", "https://amazon.com.au/dp/B000?tag=x", "https://amazon.com.au/dp/B000"},
		{"amazon subdomain tag", "https://smile.amazon.com/dp/B000?tag=x", "https://smile.amazon.com/dp/B000"},
		{"tag kept off amazon", "https://blog.example.com/posts?tag=go", "https://blog.example.com/posts?tag=go"},
		{"tag kept on amazon lookalike", "https://amazon.example.com/x?tag=go", "https://amazon.example.com/x?tag=go"},
		{"ascsubtag linkCode", "https://amazon.com/dp/B0?ascsubtag=1&linkCode=ll1&psc=1", "https://amazon.com/dp/B0?psc=1"},
		{"irclickid aff affiliate", "https://shop.example.com/p?irclickid=1&aff=2&affiliate=3&sku=9", "https://shop.example.com/p?sku=9"},

		// github
		{"github lowercase owner repo", "https://github.com/Sindresorhus/Awesome", "https://github.com/sindresorhus/awesome"},
		{"github strip .git", "https://github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"github www and http", "http://www.github.com/Foo/Bar/", "https://github.com/foo/bar"},
		{"github deeper path case kept", "https://github.com/Foo/Bar/blob/Main/README.md", "https://github.com/foo/bar/blob/Main/README.md"},
		{"github .git with slash", "https://github.com/Foo/Bar.git/", "https://github.com/foo/bar"},
		{"github owner only", "https://github.com/Foo", "https://github.com/foo"},
		{"non-github path not lowercased", "https://gitlab.com/Foo/Bar.git", "https://gitlab.com/Foo/Bar.git"},

		// youtube
		{"youtu.be", "https://youtu.be/dQw4w9WgXcQ", "https://youtube.com/watch?v=dQw4w9WgXcQ"},
		{"youtu.be with t and si", "https://youtu.be/dQw4w9WgXcQ?t=42&si=abc", "https://youtube.com/watch?v=dQw4w9WgXcQ"},
		{"watch drops list feature", "https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PL1&feature=share&t=3", "https://youtube.com/watch?v=dQw4w9WgXcQ"},
		{"watch drops unknown params", "https://youtube.com/watch?index=3&v=dQw4w9WgXcQ&pp=x", "https://youtube.com/watch?v=dQw4w9WgXcQ"},
		{"mobile youtube", "http://m.youtube.com/watch?v=dQw4w9WgXcQ", "https://youtube.com/watch?v=dQw4w9WgXcQ"},
		{"shorts", "https://www.youtube.com/shorts/abcDEF12345", "https://youtube.com/watch?v=abcDEF12345"},
		{"embed", "https://www.youtube.com/embed/dQw4w9WgXcQ?start=10", "https://youtube.com/watch?v=dQw4w9WgXcQ"},
		{"youtube uppercase host", "https://WWW.YouTube.com/watch?v=dQw4w9WgXcQ#t=1", "https://youtube.com/watch?v=dQw4w9WgXcQ"},
		{"youtube channel untouched", "https://www.youtube.com/@Channel/videos/", "https://youtube.com/@Channel/videos"},
		{"youtube watch without v", "https://youtube.com/watch?list=PL1", "https://youtube.com/watch?list=PL1"},
		{"music youtube not mapped", "https://music.youtube.com/watch?v=dQw4w9WgXcQ&si=x", "https://music.youtube.com/watch?si=x&v=dQw4w9WgXcQ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeURL(tt.in)
			if err != nil {
				t.Fatalf("NormalizeURL(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeURL(%q)\n got  %q\n want %q", tt.in, got, tt.want)
			}
			again, err := NormalizeURL(got)
			if err != nil || again != got {
				t.Fatalf("not idempotent: NormalizeURL(%q) = %q, %v", got, again, err)
			}
		})
	}
}

func TestNormalizeURLInvalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"ftp", "ftp://example.com/file"},
		{"javascript", "javascript:alert(1)"},
		{"mailto", "mailto:a@example.com"},
		{"data", "data:text/html,hi"},
		{"relative", "/just/a/path"},
		{"anchor", "#section"},
		{"no host", "https:///path"},
		{"opaque", "https:example.com"},
		{"credentials", "https://user:pass@example.com/"},
		{"username only", "https://user@example.com/"},
		{"too long", "https://example.com/" + strings.Repeat("a", 2048)},
		{"bad escape", "https://example.com/%zz"},
		{"empty host with port", "https://:443/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeURL(tt.in)
			if !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("NormalizeURL(%q) = %q, %v; want ErrInvalidURL", tt.in, got, err)
			}
		})
	}
}

func TestNormalizeURLMaxLengthBoundary(t *testing.T) {
	base := "https://example.com/"
	ok := base + strings.Repeat("a", 2048-len(base))
	if _, err := NormalizeURL(ok); err != nil {
		t.Fatalf("2048-char URL rejected: %v", err)
	}
}

func TestDomain(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://example.com/a", "example.com"},
		{"https://docs.example.com:8443/a", "docs.example.com"},
		{"https://WWW.Example.com/", "example.com"},
		{"https://[2606:4700::1]/a", "2606:4700::1"},
		{"::not a url", ""},
	}
	for _, tt := range tests {
		if got := Domain(tt.in); got != tt.want {
			t.Errorf("Domain(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGitHubRepo(t *testing.T) {
	tests := []struct {
		in          string
		owner, repo string
		ok          bool
	}{
		{"https://github.com/sindresorhus/awesome", "sindresorhus", "awesome", true},
		{"https://github.com/golang/go", "golang", "go", true},
		{"https://github.com/Foo/Bar", "foo", "bar", true},
		{"https://github.com/foo", "", "", false},
		{"https://github.com/", "", "", false},
		{"https://github.com/foo/bar/issues", "", "", false},
		{"https://github.com/foo/bar?tab=readme", "", "", false},
		{"https://github.com/topics/go", "", "", false},
		{"https://github.com/orgs/foo", "", "", false},
		{"https://github.com/sponsors/foo", "", "", false},
		{"https://github.com/marketplace/actions", "", "", false},
		{"https://github.com/features/actions", "", "", false},
		{"https://github.com/about/careers", "", "", false},
		{"https://github.com/login/oauth", "", "", false},
		{"https://github.com/settings/tokens", "", "", false},
		{"https://github.com/apps/dependabot", "", "", false},
		{"https://github.com/collections/devops", "", "", false},
		{"https://github.com/trending/go", "", "", false},
		{"https://gitlab.com/foo/bar", "", "", false},
		{"https://gist.github.com/foo/bar", "", "", false},
	}
	for _, tt := range tests {
		owner, repo, ok := GitHubRepo(tt.in)
		if owner != tt.owner || repo != tt.repo || ok != tt.ok {
			t.Errorf("GitHubRepo(%q) = %q, %q, %v; want %q, %q, %v", tt.in, owner, repo, ok, tt.owner, tt.repo, tt.ok)
		}
	}
}

func TestGitHubRepoAfterNormalize(t *testing.T) {
	n, err := NormalizeURL("http://www.GitHub.com/Owner/Repo.git/#readme")
	if err != nil {
		t.Fatal(err)
	}
	owner, repo, ok := GitHubRepo(n)
	if !ok || owner != "owner" || repo != "repo" {
		t.Fatalf("GitHubRepo(%q) = %q, %q, %v", n, owner, repo, ok)
	}
}

func TestYouTubeID(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ", true},
		{"https://youtube.com/watch?list=PL1", "", false},
		{"https://youtube.com/@channel", "", false},
		{"https://example.com/watch?v=dQw4w9WgXcQ", "", false},
		{"https://youtube.com/watch?v=bad<id>", "", false},
	}
	for _, tt := range tests {
		got, ok := YouTubeID(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("YouTubeID(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
	n, _ := NormalizeURL("https://youtu.be/abc_DEF-123?si=x")
	if id, ok := YouTubeID(n); !ok || id != "abc_DEF-123" {
		t.Errorf("YouTubeID(normalized youtu.be) = %q, %v", id, ok)
	}
}

func TestIsShortener(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"bit.ly", true},
		{"BIT.LY", true},
		{"www.bit.ly", true},
		{"tinyurl.com", true},
		{"t.co", true},
		{"goo.gl", true},
		{"ow.ly", true},
		{"is.gd", true},
		{"buff.ly", true},
		{"cutt.ly", true},
		{"rebrand.ly", true},
		{"shorturl.at", true},
		{"tiny.cc", true},
		{"rb.gy", true},
		{"lnkd.in", true},
		{"s.id", true},
		{"t.ly", true},
		{"bl.ink", true},
		{"short.io", true},
		{"amzn.to", true},
		{"example.com", false},
		{"github.com", false},
		{"youtu.be", false},
		{"notbit.ly", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsShortener(tt.in); got != tt.want {
			t.Errorf("IsShortener(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsAmazonHost(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"amazon.com", true},
		{"amazon.co.uk", true},
		{"amazon.de", true},
		{"amazon.co.jp", true},
		{"amazon.com.br", true},
		{"smile.amazon.com", true},
		{"amazon.example.com", false},
		{"notamazon.com", false},
		{"amazonaws.com", false},
	}
	for _, tt := range tests {
		if got := isAmazonHost(tt.in); got != tt.want {
			t.Errorf("isAmazonHost(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
