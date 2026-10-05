package stats

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/a-h/templ"
	"github.com/starfederation/datastar-go/datastar"
)

const (
	// liveThrottle is the shortest time between two patches of one stream.
	liveThrottle = 1500 * time.Millisecond
	// liveHeartbeat is how often an idle stream sends a comment, so
	// proxies and clients keep the connection open.
	liveHeartbeat = 15 * time.Second
)

// liveURL is u with live=1, the request that opens the stream for the page
// at u.
func liveURL(u *url.URL) string {
	q := u.Query()
	q.Set("live", "1")
	c := *u
	c.RawQuery = q.Encode()
	return c.RequestURI()
}

// live answers a request with live=1 by holding the response open as a
// Datastar event stream. It patches #page from rt's render function at
// once, so a reconnecting client catches up, and again after each write
// the Store commits, with the window ending now. Patches are at least
// liveThrottle apart, and writes that land in between share the next one.
// It returns when the client goes away.
func (s *Server) live(w http.ResponseWriter, r *http.Request, rt route) {
	ctx := r.Context()
	changed := s.Store.Changed()
	body, err := s.liveBody(rt, r)
	if err != nil {
		failRender(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	rc := http.NewResponseController(w)
	if sse.PatchElementTempl(pageBody(rt.title, body)) != nil {
		return
	}
	last := time.Now()
	heartbeat := time.NewTicker(liveHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-changed:
			if !sleep(ctx, liveThrottle-time.Since(last)) {
				return
			}
			changed = s.Store.Changed()
			body, err := s.liveBody(rt, r)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Error("stats live", "path", r.URL.Path, "err", err)
				continue
			}
			if sse.PatchElementTempl(pageBody(rt.title, body)) != nil {
				return
			}
			last = time.Now()
		}
	}
}

func (s *Server) liveBody(rt route, r *http.Request) (templ.Component, error) {
	f, _ := s.filter(r)
	return rt.render(s, r, f)
}

// sleep waits d and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// failRender answers a render that failed before the stream started.
func failRender(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNotFound) {
		http.NotFound(w, r)
		return
	}
	slog.Error("stats live", "path", r.URL.Path, "err", err)
	http.Error(w, "query failed", http.StatusInternalServerError)
}
