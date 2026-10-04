package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/mach4-braai/gauger-server/internal/reconcile"
	"github.com/mach4-braai/gauger-server/internal/runner"
	"github.com/mach4-braai/gauger-server/internal/store"
)

type Handler struct {
	Store      *store.Store
	Reconciler *reconcile.Reconciler
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
		if id, err = bind(r, id); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		jobID := id.CheckRunID
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
		id, err := bind(r, p.Identity)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		p.Identity = id
		byJob[id] = append(byJob[id], p)
	}
	for id, pts := range byJob {
		jobID := id.CheckRunID
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

// bind checks id against the claims Auth.Wrap verified for r.
func bind(r *http.Request, id runner.Identity) (runner.Identity, error) {
	c, ok := ClaimsFrom(r.Context())
	if !ok {
		return id, errors.New("request carries no verified token claims")
	}
	return c.bind(id)
}
