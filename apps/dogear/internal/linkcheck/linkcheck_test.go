package linkcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(200)
		case "/gone":
			w.WriteHeader(410)
		case "/missing":
			w.WriteHeader(404)
		case "/forbidden":
			w.WriteHeader(403)
		case "/busy":
			w.WriteHeader(503)
		case "/nohead":
			if r.Method == http.MethodHead {
				w.WriteHeader(405)
				return
			}
			w.WriteHeader(200)
		case "/headlies":
			if r.Method == http.MethodHead {
				w.WriteHeader(404)
				return
			}
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	cases := map[string]Outcome{
		"/ok": OK, "/gone": Fail, "/missing": Fail, "/forbidden": Unknown, "/busy": Unknown,
		"/nohead": OK, "/headlies": OK,
	}
	for path, want := range cases {
		if got := Check(context.Background(), srv.Client(), srv.URL+path); got != want {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
	// A closed port refuses connections, which counts as a failure.
	closed := httptest.NewServer(http.NotFoundHandler())
	u := closed.URL
	closed.Close()
	if got := Check(context.Background(), http.DefaultClient, u); got != Fail {
		t.Errorf("refused: got %v", got)
	}
}
