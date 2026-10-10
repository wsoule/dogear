package ghsync

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Webhook event kinds returned by ParseWebhook.
const (
	KindRevoke = "revoke"
	KindIgnore = "ignore"
)

// VerifySignature checks an X-Hub-Signature-256 header ("sha256=<hex>")
// against the HMAC-SHA256 of body under secret, using a constant-time compare.
func VerifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	hexSig, ok := strings.CutPrefix(strings.TrimSpace(header), "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// WebhookEvent is the part of a GitHub App webhook the sync cares about.
type WebhookEvent struct {
	// Kind is "revoke" or "ignore". For installation_repositories "removed"
	// events Kind is "revoke" and RemovedRepos lists the removed repos; the
	// caller revokes only if the user's synced repo is among them.
	Kind           string
	InstallationID int64
	RemovedRepos   []string
}

// ParseWebhook interprets an X-GitHub-Event type and its JSON body.
// "installation" with action deleted or suspend revokes the installation;
// "installation_repositories" with action removed reports the removed repos.
// Every other event is ignored.
func ParseWebhook(eventType string, body []byte) (WebhookEvent, error) {
	var payload struct {
		Action       string `json:"action"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		RepositoriesRemoved []struct {
			FullName string `json:"full_name"`
		} `json:"repositories_removed"`
	}
	switch eventType {
	case "installation", "installation_repositories":
	default:
		return WebhookEvent{Kind: KindIgnore}, nil
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return WebhookEvent{}, fmt.Errorf("ghsync: parse %s webhook: %w", eventType, err)
	}
	ev := WebhookEvent{Kind: KindIgnore, InstallationID: payload.Installation.ID}
	switch {
	case eventType == "installation" && (payload.Action == "deleted" || payload.Action == "suspend"):
		ev.Kind = KindRevoke
	case eventType == "installation_repositories" && payload.Action == "removed":
		for _, r := range payload.RepositoriesRemoved {
			ev.RemovedRepos = append(ev.RemovedRepos, r.FullName)
		}
		if len(ev.RemovedRepos) > 0 {
			ev.Kind = KindRevoke
		}
	}
	return ev, nil
}
