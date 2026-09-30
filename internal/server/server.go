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
	// Runner handles gauger's lifecycle and OTLP routes, already wrapped in
	// runner authentication.
	Runner http.Handler
	// Webhooks handles POST /webhooks/github.
	Webhooks http.Handler
	// UI handles every other path on the UI listener.
	UI http.Handler
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Write([]byte("ok\n"))
}

// RunnerHandler serves the tailnet-only :4318 listener.
func (s *Server) RunnerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("/v1/", s.Runner)
	return mux
}

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
	Runner, UI, Webhook net.Listener
}

// Serve runs one http.Server per listener until ctx ends or one fails.
func (s *Server) Serve(ctx context.Context, l Listeners) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, x := range []struct {
		name string
		ln   net.Listener
		h    http.Handler
	}{
		{"runner", l.Runner, s.RunnerHandler()},
		{"ui", l.UI, s.UIHandler()},
		{"webhook", l.Webhook, s.WebhookHandler()},
	} {
		srv := &http.Server{
			Handler:           x.h,
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
