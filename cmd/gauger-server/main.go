package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mach4-braai/gauger-server/internal/config"
	"github.com/mach4-braai/gauger-server/internal/server"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/tailnet"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("gauger-server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
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

	node, err := tailnet.Join(ctx, cfg.TSDir, cfg.TSHostname, cfg.TSAuthKey)
	if err != nil {
		return err
	}
	defer node.Close()
	slog.Info("joined tailnet", "dns_name", node.DNSName)

	srv := &server.Server{Store: st}
	return srv.Serve(ctx, server.Listeners{Runner: node.Runner, UI: node.UI, Webhook: node.Webhook})
}
