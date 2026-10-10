package service

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestFetchTranscriptLive hits YouTube; run with DOGEAR_LIVE=1.
func TestFetchTranscriptLive(t *testing.T) {
	if os.Getenv("DOGEAR_LIVE") == "" {
		t.Skip("set DOGEAR_LIVE=1 to call YouTube")
	}
	s := &Service{API: &http.Client{Timeout: 15 * time.Second}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	lang, text, err := s.FetchTranscript(context.Background(), "dQw4w9WgXcQ")
	if err != nil || len(text) < 200 {
		t.Fatalf("lang=%q len=%d err=%v", lang, len(text), err)
	}
	t.Logf("lang=%s len=%d start=%q", lang, len(text), text[:80])
}

func TestXMLText(t *testing.T) {
	got := xmlText([]byte(`<timedtext format="3"><body><p t="1"><s>hello</s><s> world</s></p><p>again</p></body></timedtext>`))
	if len(got) != 3 || got[0] != "hello" || got[2] != "again" {
		t.Fatalf("got %q", got)
	}
}
