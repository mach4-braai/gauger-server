package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/mach4-braai/gauger-server/internal/config"
	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/ingest"
	"github.com/mach4-braai/gauger-server/internal/reconcile"
	"github.com/mach4-braai/gauger-server/internal/server"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/tailnet"
	"github.com/mach4-braai/gauger-server/internal/ui"
	"github.com/mach4-braai/gauger-server/internal/webhook"
)

// version is set at link time with -ldflags "-X main.version=...".
var version string

func resolveVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("gauger-server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	v := resolveVersion()
	slog.Info("starting", "version", v)
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.Retention)
	if err != nil {
		return err
	}
	defer st.Close()
	go st.RunMaintenance(ctx)

	var creds github.CredentialSource = st
	if cfg.GitHubApp != nil {
		creds = github.StaticCredentials{C: cfg.GitHubApp}
	}
	gh := github.NewClient(cfg.GitHubAPIURL, creds)
	rec := reconcile.New(st, gh)
	go rec.Run(ctx)

	node, err := tailnet.Join(ctx, cfg.TSDir, cfg.TSHostname, cfg.TSAuthKey)
	if err != nil {
		return err
	}
	defer node.Close()
	slog.Info("joined tailnet", "dns_name", node.DNSName)

	auth := &ingest.Auth{
		Tags:     node,
		Tag:      cfg.RunnerTag,
		Verifier: ingest.NewVerifier(ctx, cfg.OIDCAudience),
		OwnerID:  cfg.OIDCOwnerID,
	}
	srv := &server.Server{
		Runner:   auth.Wrap((&ingest.Handler{Store: st, Reconciler: rec}).Routes()),
		Webhooks: &webhook.Handler{Store: st, Creds: creds, Wake: rec.Wake},
		UI: (&ui.UI{
			Store: st, GitHub: gh, Creds: creds, Rates: cfg.RunnerRates,
			DNSName: node.DNSName, GitHubURL: cfg.GitHubURL, Wake: rec.Wake, Version: v,
		}).Handler(),
	}
	return srv.Serve(ctx, server.Listeners{Runner: node.Runner, UI: node.UI, Webhook: node.Webhook})
}
