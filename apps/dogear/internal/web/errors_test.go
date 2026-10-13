package web

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCanceledRequestIsNotA500 checks that a query canceled because the
// client went away logs at info and answers 499 instead of an error and 500.
func TestCanceledRequestIsNotA500(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logs, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/search?q=go", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.serverError(w, r, fmt.Errorf("feed query: %w", context.Canceled))
	if w.Code != statusClientClosed {
		t.Errorf("status %d, want %d", w.Code, statusClientClosed)
	}
	if w.Body.Len() != 0 {
		t.Errorf("wrote a body: %q", w.Body.String())
	}
	if out := logs.String(); strings.Contains(out, "level=ERROR") || !strings.Contains(out, "level=INFO") {
		t.Errorf("logs = %q, want one info line", out)
	}
}
