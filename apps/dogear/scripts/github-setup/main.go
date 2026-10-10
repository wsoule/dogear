// Command github-setup creates Dogear's GitHub App with GitHub's manifest
// flow and stores its credentials in Railway. One GitHub App provides both
// "Sign in with GitHub" (its OAuth client id/secret) and list-repo sync.
//
//	go run ./scripts/github-setup -base https://dogear.fyi
//
// It opens a local page that posts a pre-filled app manifest to GitHub. You
// review it and click "Create GitHub App"; GitHub redirects back here with a
// one-time code, which is exchanged for the app's id, private key, webhook
// secret, and OAuth credentials. Those are written to the Railway "web"
// service with `railway variables --set` and never printed.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

var page = template.Must(template.New("p").Parse(`<!doctype html><meta charset="utf-8">
<title>Create the Dogear GitHub App</title>
<body style="font:14px ui-monospace,monospace;max-width:560px;margin:60px auto">
<p>Sending you to GitHub to create the app. Review it and click <b>Create GitHub App</b>.</p>
<form id="f" method="post" action="https://github.com/settings/apps/new?state={{.State}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<button type="submit">continue to GitHub</button></form>
<script>document.getElementById("f").submit()</script></body>`))

func main() {
	base := flag.String("base", "", "public base URL of the site, e.g. https://dogear.fyi")
	name := flag.String("name", "Dogear Sync", "GitHub App name (must be unique on GitHub)")
	service := flag.String("service", "web", "Railway service to store the credentials on")
	flag.Parse()
	if *base == "" {
		fmt.Fprintln(os.Stderr, "usage: github-setup -base https://dogear.fyi")
		os.Exit(2)
	}
	*base = strings.TrimRight(*base, "/")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail(err)
	}
	local := "http://" + ln.Addr().String()
	stateBytes := make([]byte, 16)
	_, _ = rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)

	manifest, _ := json.Marshal(map[string]any{
		"name":                     *name,
		"url":                      *base,
		"description":              "Sign in to Dogear and sync your public lists to an awesome-<you> repo.",
		"redirect_url":             local + "/callback",
		"callback_urls":            []string{*base + "/auth/github/callback"},
		"setup_url":                *base + "/github/setup",
		"setup_on_update":          true,
		"request_oauth_on_install": false,
		"public":                   true,
		"hook_attributes":          map[string]any{"url": *base + "/webhooks/github", "active": true},
		"default_permissions":      map[string]string{"contents": "write", "metadata": "read"},
		"default_events":           []string{},
	})

	done := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_ = page.Execute(w, map[string]string{"State": state, "Manifest": string(manifest)})
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		err := convert(r.Context(), r.URL.Query().Get("code"), *service)
		if err != nil {
			http.Error(w, "setup failed: "+err.Error(), http.StatusBadGateway)
		} else {
			fmt.Fprint(w, "Done. The Dogear GitHub App is created and its credentials are stored in Railway. You can close this tab.")
		}
		done <- err
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	fmt.Println("Opening", local, "— click \"Create GitHub App\" on GitHub.")
	_ = exec.Command("open", local).Start()
	select {
	case err = <-done:
	case <-time.After(15 * time.Minute):
		err = fmt.Errorf("timed out waiting for GitHub")
	}
	_ = srv.Shutdown(context.Background())
	if err != nil {
		fail(err)
	}
	fmt.Println("GitHub App created; credentials stored on Railway service", *service)
}

// convert exchanges the manifest code for the app's credentials and writes
// them to Railway. The private key's newlines are stored as literal \n,
// which the server expands.
func convert(ctx context.Context, code, service string) error {
	if code == "" {
		return fmt.Errorf("missing code")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/app-manifests/"+code+"/conversions", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("github conversion: status %d", resp.StatusCode)
	}
	var app struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		PEM           string `json:"pem"`
	}
	if err := json.Unmarshal(body, &app); err != nil {
		return err
	}
	vars := []string{
		"GITHUB_CLIENT_ID=" + app.ClientID,
		"GITHUB_CLIENT_SECRET=" + app.ClientSecret,
		fmt.Sprintf("GITHUB_APP_ID=%d", app.ID),
		"GITHUB_APP_SLUG=" + app.Slug,
		"GITHUB_APP_WEBHOOK_SECRET=" + app.WebhookSecret,
		"GITHUB_APP_PRIVATE_KEY=" + strings.ReplaceAll(strings.TrimSpace(app.PEM), "\n", `\n`),
	}
	args := []string{"variables", "--service", service}
	for _, v := range vars {
		args = append(args, "--set", v)
	}
	cmd := exec.CommandContext(ctx, "railway", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("railway variables: %w", err)
	}
	fmt.Println("App:", "https://github.com/apps/"+app.Slug)
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
