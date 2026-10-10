package web

import "testing"

func TestLocalPath(t *testing.T) {
	ok := []string{"/", "/c/go", "/c/go?sort=new&kind=tool"}
	for _, p := range ok {
		if got := localPath(p); got != p {
			t.Errorf("localPath(%q) = %q, want unchanged", p, got)
		}
	}
	bad := []string{"", "//evil.com", "/\\evil.com", "/\t/evil.com", "/\n/evil.com", "https://evil.com", "evil.com", "/\\/evil.com"}
	for _, p := range bad {
		if got := localPath(p); got != "/" {
			t.Errorf("localPath(%q) = %q, want /", p, got)
		}
	}
}
