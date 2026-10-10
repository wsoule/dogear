package ghsync

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
)

func sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"action":"deleted"}`)
	good := sign(secret, body)
	cases := []struct {
		name   string
		secret []byte
		body   []byte
		header string
		want   bool
	}{
		{"valid", secret, body, good, true},
		{"wrong secret", []byte("other"), body, good, false},
		{"tampered body", secret, []byte(`{"action":"created"}`), good, false},
		{"missing prefix", secret, body, good[len("sha256="):], false},
		{"sha1 header", secret, body, "sha1=abc", false},
		{"not hex", secret, body, "sha256=zz", false},
		{"empty", secret, body, "", false},
		{"empty secret", nil, body, sign(nil, body), false},
	}
	for _, c := range cases {
		if got := VerifySignature(c.secret, c.body, c.header); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestParseWebhook(t *testing.T) {
	cases := []struct {
		name, event, body string
		want              WebhookEvent
	}{
		{"deleted", "installation", `{"action":"deleted","installation":{"id":5}}`, WebhookEvent{Kind: "revoke", InstallationID: 5}},
		{"suspend", "installation", `{"action":"suspend","installation":{"id":5}}`, WebhookEvent{Kind: "revoke", InstallationID: 5}},
		{"created", "installation", `{"action":"created","installation":{"id":5}}`, WebhookEvent{Kind: "ignore", InstallationID: 5}},
		{"removed", "installation_repositories", `{"action":"removed","installation":{"id":6},"repositories_removed":[{"full_name":"ada/awesome-ada"},{"full_name":"ada/x"}]}`,
			WebhookEvent{Kind: "revoke", InstallationID: 6, RemovedRepos: []string{"ada/awesome-ada", "ada/x"}}},
		{"added", "installation_repositories", `{"action":"added","installation":{"id":6},"repositories_added":[{"full_name":"ada/y"}]}`, WebhookEvent{Kind: "ignore", InstallationID: 6}},
		{"ping", "ping", `{"zen":"hi"}`, WebhookEvent{Kind: "ignore"}},
	}
	for _, c := range cases {
		got, err := ParseWebhook(c.event, []byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Kind != c.want.Kind || got.InstallationID != c.want.InstallationID || !slices.Equal(got.RemovedRepos, c.want.RemovedRepos) {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	if _, err := ParseWebhook("installation", []byte("{")); err == nil {
		t.Error("expected error for malformed body")
	}
}
