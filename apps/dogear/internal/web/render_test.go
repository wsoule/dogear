package web

import (
	"io"
	"testing"
)

// TestTemplatesExecute renders every page's layout with empty data so
// escaping or parse problems fail in tests instead of as a 500.
func TestTemplatesExecute(t *testing.T) {
	ts, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for name, tmpl := range ts {
		if err := tmpl.ExecuteTemplate(io.Discard, "layout", &Page{Nav: "top", BaseURL: "https://dogear.fyi"}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
