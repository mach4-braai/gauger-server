// Package server builds one HTTP handler per listener and serves them.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"
)

type Server struct {
	// Runner handles gauger's lifecycle and OTLP routes on the tailnet,
	// already wrapped in runner authentication.
	Runner http.Handler
	// FunnelRunner handles the same routes from the internet, already
	// wrapped in runner authentication without the tailnet tag check.
	FunnelRunner http.Handler
	// RunnerConnContext, if set, adds a runner connection's details to the
	// context of its requests.
	RunnerConnContext func(context.Context, net.Conn) context.Context
	// Webhooks handles POST /webhooks/github.
	Webhooks http.Handler
	// UI handles every other path on the UI listener.
	UI http.Handler
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Write([]byte("ok\n"))
}

func runnerMux(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("/v1/", h)
	return mux
}

// RunnerHandler serves the tailnet-only :4318 listener.
func (s *Server) RunnerHandler() http.Handler { return runnerMux(s.Runner) }

// FunnelRunnerHandler serves the Funnel-only :10000 listener.
func (s *Server) FunnelRunnerHandler() http.Handler { return runnerMux(s.FunnelRunner) }

// UIHandler serves the tailnet-only :443 listener.
func (s *Server) UIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("/", s.UI)
	return mux
}

// WebhookHandler serves the Funnel-only :8443 listener.
func (s *Server) WebhookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("POST /webhooks/github", s.Webhooks)
	return mux
}

type Listeners struct {
	Runner, FunnelRunner, UI, Webhook net.Listener
}

// Serve runs one http.Server per listener until ctx ends or one fails.
func (s *Server) Serve(ctx context.Context, l Listeners) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, x := range []struct {
		name    string
		ln      net.Listener
		h       http.Handler
		connCtx func(context.Context, net.Conn) context.Context
	}{
		{"runner", l.Runner, s.RunnerHandler(), s.RunnerConnContext},
		{"funnel-runner", l.FunnelRunner, s.FunnelRunnerHandler(), s.RunnerConnContext},
		{"ui", l.UI, s.UIHandler(), nil},
		{"webhook", l.Webhook, s.WebhookHandler(), nil},
	} {
		srv := &http.Server{
			Handler:           x.h,
			ConnContext:       x.connCtx,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
		g.Go(func() error {
			slog.Info("serving", "listener", x.name, "addr", x.ln.Addr().String())
			if err := srv.Serve(x.ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		})
	}
	return g.Wait()
}
