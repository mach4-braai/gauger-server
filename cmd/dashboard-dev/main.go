// Command dashboard-dev serves the /stats/ dashboard on a loopback address
// for local preview. It needs only GAUGER_DATABASE_URL: no tailnet, no
// GitHub credentials, and no setup, webhook or ingest routes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/stats"
)

func main() {
	if err := run(); err != nil {
		slog.Error("dashboard-dev stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8090", "loopback address to listen on")
	seed := flag.Bool("seed", false, "fill an empty database with the test fixtures")
	flag.Parse()

	if err := loopback(*addr); err != nil {
		return err
	}
	dbURL := os.Getenv("GAUGER_DATABASE_URL")
	if dbURL == "" {
		return errors.New("GAUGER_DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dbURL, 90*24*time.Hour)
	if err != nil {
		return err
	}
	defer st.Close()
	if *seed {
		if err := fill(ctx, st); err != nil {
			return err
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: (&stats.Server{
			Store: st, Rates: spend.DefaultRates, GitHubURL: "https://github.com", Version: "dev",
			FeedbackURL: github.NewIssueURL("https://github.com", "mach4-braai/gauger-server"),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	slog.Info("serving the dashboard", "url", "http://"+ln.Addr().String()+"/stats/")
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loopback refuses any address but a loopback IP, so the dashboard, which
// has no authentication, is never reachable from another machine.
func loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-addr %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-addr %q: refusing to listen on anything but a loopback IP such as 127.0.0.1", addr)
	}
	return nil
}

// fill seeds the fixtures unless the database already has jobs.
func fill(ctx context.Context, st *store.Store) error {
	var jobs int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&jobs); err != nil {
		return err
	}
	if jobs > 0 {
		slog.Info("not seeding: the database already has jobs", "jobs", jobs)
		return nil
	}
	fx, err := storetest.Fill(ctx, st, time.Now())
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	slog.Info("seeded the fixtures", "repositories", len(fx.Repositories))
	return nil
}
