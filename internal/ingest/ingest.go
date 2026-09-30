package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/reconcile"
	"github.com/mach4-braai/gauger-server/internal/runner"
	"github.com/mach4-braai/gauger-server/internal/store"
)

// resyncInterval stops a burst of unmatched batches from repeating the same
// REST lookup.
const resyncInterval = 30 * time.Second

var errUnmatched = errors.New("no job matches this runner yet")

type Handler struct {
	Store      *store.Store
	Reconciler *reconcile.Reconciler

	mu       sync.Mutex
	syncedAt map[string]time.Time
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs/start", h.lifecycle(false))
	mux.HandleFunc("POST /v1/jobs/done", h.lifecycle(true))
	mux.HandleFunc("POST /v1/metrics", h.metrics)
	return mux
}

func (h *Handler) lifecycle(done bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "body must be a JSON object", http.StatusBadRequest)
			return
		}
		id, err := runner.ParseIdentity(func(k string) string {
			switch v := body[k].(type) {
			case string:
				return v
			case json.Number:
				return v.String()
			}
			return ""
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		at := time.Now()
		if s, ok := body["time"].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				at = t
			}
		}
		jobID, err := h.resolve(r.Context(), id, at)
		if err != nil {
			h.resolveError(w, id, err)
			return
		}
		if err := h.Store.MarkRunnerSeen(r.Context(), jobID, id, done); err != nil {
			slog.Error("mark runner seen", "job_id", jobID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h.Reconciler.Wake()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"job_id\":%d}\n", jobID)
	}
}

func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	req, err := runner.DecodeRequest(http.MaxBytesReader(w, r.Body, runner.MaxBody), ct, r.Header.Get("Content-Encoding"))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, runner.ErrTooLarge) || errors.As(err, &tooLarge):
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	points, rejected := runner.Points(req)
	byJob := map[runner.Identity][]runner.Point{}
	for _, p := range points {
		byJob[p.Identity] = append(byJob[p.Identity], p)
	}
	for id, pts := range byJob {
		jobID, err := h.resolve(r.Context(), id, pts[0].Time)
		if err != nil {
			h.resolveError(w, id, err)
			return
		}
		if err := h.Store.MarkRunnerSeen(r.Context(), jobID, id, false); err != nil {
			slog.Error("mark runner seen", "job_id", jobID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		dropped, err := h.Store.InsertSamples(r.Context(), jobID, pts)
		if err != nil {
			slog.Error("insert samples", "job_id", jobID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		rejected += dropped
	}
	h.Reconciler.Wake()

	resp := &colmetrics.ExportMetricsServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &colmetrics.ExportMetricsPartialSuccess{
			RejectedDataPoints: rejected,
			ErrorMessage:       "points need a gauge or sum value, the gauger identity attributes, and a time inside the retention window",
		}
	}
	var out []byte
	if runner.IsJSON(ct) {
		out, err = protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
	} else {
		out, err = proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(out)
}

// resolve returns the job ID for an identity. Without a check_run_id it
// matches runner.name to the job that runner was running at time at,
// refreshing the run's jobs from REST once if none matches.
func (h *Handler) resolve(ctx context.Context, id runner.Identity, at time.Time) (int64, error) {
	if id.CheckRunID != 0 {
		return id.CheckRunID, nil
	}
	jobID, ok, err := h.Store.MatchJobByRunner(ctx, id.RunID, id.RunAttempt, id.RunnerName, at)
	if err != nil || ok {
		return jobID, err
	}
	if !h.claimSync(store.RunKey(id.RunID, id.RunAttempt)) {
		return 0, errUnmatched
	}
	if _, err := h.Reconciler.SyncRunAttempt(ctx, id.Repository, id.RunID, id.RunAttempt); err != nil {
		return 0, err
	}
	jobID, ok, err = h.Store.MatchJobByRunner(ctx, id.RunID, id.RunAttempt, id.RunnerName, at)
	if err == nil && !ok {
		err = errUnmatched
	}
	return jobID, err
}

func (h *Handler) claimSync(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.syncedAt == nil {
		h.syncedAt = map[string]time.Time{}
	}
	now := time.Now()
	for k, t := range h.syncedAt {
		if now.Sub(t) > resyncInterval {
			delete(h.syncedAt, k)
		}
	}
	if _, recent := h.syncedAt[key]; recent {
		return false
	}
	h.syncedAt[key] = now
	return true
}

func (h *Handler) resolveError(w http.ResponseWriter, id runner.Identity, err error) {
	var rl *github.RateLimitError
	retry := 30 * time.Second
	switch {
	case errors.As(err, &rl):
		retry = max(time.Until(rl.Until), time.Second)
	case errors.Is(err, errUnmatched), errors.Is(err, github.ErrNotConfigured):
	case github.IsNotFound(err):
		http.Error(w, "the GitHub App does not cover "+id.Repository, http.StatusUnprocessableEntity)
		return
	default:
		slog.Error("resolve runner job", "run_id", id.RunID, "runner", id.RunnerName, "err", err)
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
	http.Error(w, "job not known yet: "+err.Error(), http.StatusServiceUnavailable)
}
