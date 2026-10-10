// Command github-app-hook points the GitHub App's webhook at a new base URL
// after a domain change. It reads GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY
// from the environment (run it with `railway run --service web`).
//
//	go run ./scripts/github-app-hook -base https://dogear.fyi
//
// GitHub's API cannot change an app's OAuth callback or setup URLs; update
// those in the app's settings page.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"dogear/internal/ghsync"
)

func main() {
	base := flag.String("base", "", "new public base URL")
	flag.Parse()
	id, err := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
	if err != nil || *base == "" {
		fmt.Fprintln(os.Stderr, "usage: GITHUB_APP_ID=… GITHUB_APP_PRIVATE_KEY=… github-app-hook -base https://dogear.fyi")
		os.Exit(2)
	}
	app := &ghsync.App{AppID: id, PrivateKeyPEM: []byte(strings.ReplaceAll(os.Getenv("GITHUB_APP_PRIVATE_KEY"), `\n`, "\n"))}
	jwt, err := app.JWT(time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "jwt:", err)
		os.Exit(1)
	}
	body, _ := json.Marshal(map[string]string{"url": strings.TrimRight(*base, "/") + "/webhooks/github", "content_type": "json"})
	req, _ := http.NewRequest(http.MethodPatch, "https://api.github.com/app/hook/config", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "github: %d %s\n", resp.StatusCode, b)
		os.Exit(1)
	}
	fmt.Println("webhook url now", out.URL)
}
