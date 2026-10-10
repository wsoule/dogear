// Command dogear is the single binary for the Dogear site. Subcommands:
// serve (web UI, REST API, MCP), migrate, import, and linkcheck.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dogear/internal/api"
	"dogear/internal/auth"
	"dogear/internal/classify"
	"dogear/internal/config"
	"dogear/internal/db"
	"dogear/internal/ghsync"
	"dogear/internal/importer"
	"dogear/internal/linkcheck"
	"dogear/internal/mcp"
	"dogear/internal/migrations"
	"dogear/internal/safety"
	"dogear/internal/service"
	"dogear/internal/web"
)

const usage = `usage: dogear <command> [flags]

commands:
  serve       run the web UI, REST API, and MCP endpoint
  migrate     apply database migrations, then exit
  import      import the awesome lists, then exit (--force, --only owner/repo, --stars)
  linkcheck   check a batch of item URLs, then exit (--batch N)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = migrate(ctx)
	case "import":
		err = runImport(ctx, os.Args[2:])
	case "linkcheck":
		err = runLinkcheck(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	l := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(l)
	return l
}

func openPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pc.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func migrate(ctx context.Context) error {
	cfg, err := config.Load("db")
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	if err := migrations.Up(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	log.Info("migrations applied")
	return nil
}

func serve(ctx context.Context) error {
	cfg, err := config.Load("db", "web")
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	// Fallback for hosts without a pre-deploy step: migrate under an
	// advisory lock before serving. A no-op when already up to date.
	if os.Getenv("MIGRATE_ON_START") != "false" {
		if err := migrations.Up(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
	}
	pool, err := openPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	cl, err := classify.New(cfg.Classifier, cfg.JevAPIKey, cfg.JevBaseURL, nil)
	if err != nil {
		return err
	}
	svc := service.New(pool, cfg, cl, log)

	providers := map[string]auth.Provider{}
	if cfg.GitHubLogin() {
		providers["github"] = auth.NewGitHub(cfg.GitHubClientID, cfg.GitHubClientSecret, cfg.BaseURL)
	}
	if cfg.GoogleLogin() {
		providers["google"] = auth.NewGoogle(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.BaseURL)
	}
	if cfg.AppleLogin() {
		apple, err := auth.NewApple(cfg.AppleClientID, cfg.AppleTeamID, cfg.AppleKeyID, cfg.ApplePrivateKey, cfg.BaseURL)
		if err != nil {
			return err
		}
		providers["apple"] = apple
	}
	if len(providers) == 0 {
		log.Warn("no sign-in providers configured; the site is read-only")
	}

	var app *ghsync.App
	if cfg.GitHubApp() {
		app = &ghsync.App{AppID: cfg.GitHubAppID, PrivateKeyPEM: []byte(strings.ReplaceAll(cfg.GitHubAppPrivateKey, `\n`, "\n")), Slug: cfg.GitHubAppSlug}
		svc.UninstallApp = app.DeleteInstallation
		worker := &ghsync.Worker{Store: service.SyncStore{S: svc}, App: app, Log: log}
		go worker.Run(ctx)
	}
	go svc.RunBackground(ctx)
	go svc.RunLoggitSync(ctx)

	apiHandler := api.New(svc, web.SessionActor, auth.Signer{Secret: cfg.SessionSecret}, cfg.BaseURL, cfg.ProxyTrust())
	site, err := web.New(web.Options{Service: svc, Config: cfg, Log: log, Providers: providers, App: app,
		API: apiHandler, MCP: mcp.Handler(svc, cfg.BaseURL)})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              "0.0.0.0:" + cfg.Port,
		Handler:           site.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "base_url", cfg.BaseURL, "env", cfg.Env)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
	return nil
}

func runImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	force := fs.Bool("force", false, "ignore stored README hashes")
	only := fs.String("only", "", "import a single owner/repo list")
	stars := fs.Bool("stars", false, "refresh GitHub stars and priors only")
	_ = fs.Parse(args)

	cfg, err := config.Load("db")
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	if cfg.GitHubToken == "" {
		log.Warn("GITHUB_TOKEN is not set; GitHub allows only 60 unauthenticated requests per hour")
	}
	pool, err := openPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	im := &importer.Importer{Pool: pool, Log: log, GH: &importer.GitHub{Token: cfg.GitHubToken,
		HTTP: &http.Client{Timeout: 60 * time.Second}, UserAgent: "DogearBot/1.0"}}
	sum, err := im.Run(ctx, importer.Options{Force: *force, Only: *only, Stars: *stars})
	sum.Print(os.Stdout)
	return err
}

func runLinkcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("linkcheck", flag.ExitOnError)
	batch := fs.Int("batch", 2000, "items per run")
	_ = fs.Parse(args)
	cfg, err := config.Load("db")
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	pool, err := openPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	base := cfg.BaseURL
	if base == "" {
		base = "https://dogear.fyi"
	}
	c := &linkcheck.Checker{Q: db.New(pool), Log: log, Batch: *batch,
		Client: safety.NewClient(safety.ClientOptions{Timeout: 10 * time.Second, MaxRedirects: 5, UserAgent: "DogearBot/1.0 (+" + base + "/about)"})}
	sum, err := c.Run(ctx)
	fmt.Printf("checked %d: ok %d, failed %d, unknown %d, dead %d\n", sum.Checked, sum.OK, sum.Failed, sum.Unknown, sum.Dead)
	return err
}
