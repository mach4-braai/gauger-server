// Package webhook receives GitHub workflow_run and workflow_job webhooks.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/reconcile"
	"github.com/mach4-braai/gauger-server/internal/store"
)

// maxBody is GitHub's webhook payload cap.
const maxBody = 25 << 20

type Handler struct {
	Store *store.Store
	Creds github.CredentialSource
	// Wake is called after a delivery adds repair work.
	Wake func()
}

var (
	errDuplicate  = errors.New("duplicate delivery")
	errBadPayload = errors.New("bad payload")
)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	creds, err := h.Creds.Credentials(r.Context())
	if err != nil {
		slog.Warn("webhook before the App is configured", "err", err)
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	if !github.ValidSignature(creds.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	if delivery == "" || event == "" {
		http.Error(w, "missing delivery headers", http.StatusBadRequest)
		return
	}
	if event != "workflow_run" && event != "workflow_job" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	err = h.Store.InTx(r.Context(), func(tx pgx.Tx) error {
		fresh, err := store.RecordDelivery(r.Context(), tx, delivery, event)
		if err != nil {
			return err
		}
		if !fresh {
			return errDuplicate
		}
		switch event {
		case "workflow_run":
			return h.workflowRun(r.Context(), tx, body)
		default:
			return h.workflowJob(r.Context(), tx, body)
		}
	})
	switch {
	case errors.Is(err, errDuplicate):
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, errBadPayload):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		slog.Error("webhook", "event", event, "delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		if h.Wake != nil {
			h.Wake()
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) workflowRun(ctx context.Context, tx pgx.Tx, body []byte) error {
	var ev github.WorkflowRunEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Repository.FullName == "" || ev.WorkflowRun.ID == 0 {
		return errBadPayload
	}
	repo := ev.Repository.FullName
	if err := store.UpsertRepository(ctx, tx, repo, ev.Repository.Private, ev.Installation.ID); err != nil {
		return err
	}
	status, err := store.UpsertRun(ctx, tx, repo, &ev.WorkflowRun)
	if err != nil {
		return err
	}
	now := time.Now()
	next := now.Add(reconcile.RunCheckDelay)
	if status == "completed" {
		next = now.Add(time.Minute)
	}
	return store.EnqueueTask(ctx, tx, store.KindRun, store.RunKey(ev.WorkflowRun.ID, ev.WorkflowRun.RunAttempt),
		repo, next, now.Add(reconcile.TaskLifetime))
}

func (h *Handler) workflowJob(ctx context.Context, tx pgx.Tx, body []byte) error {
	var ev github.WorkflowJobEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Repository.FullName == "" || ev.WorkflowJob.ID == 0 {
		return errBadPayload
	}
	repo := ev.Repository.FullName
	if err := store.UpsertRepository(ctx, tx, repo, ev.Repository.Private, ev.Installation.ID); err != nil {
		return err
	}
	if err := store.UpsertJob(ctx, tx, repo, &ev.WorkflowJob); err != nil {
		return err
	}
	var runStatus string
	err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 AND attempt = $2`,
		ev.WorkflowJob.RunID, ev.WorkflowJob.RunAttempt).Scan(&runStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if runStatus == "completed" {
		return nil
	}
	now := time.Now()
	return store.EnqueueTask(ctx, tx, store.KindRun, store.RunKey(ev.WorkflowJob.RunID, ev.WorkflowJob.RunAttempt),
		repo, now.Add(reconcile.RunCheckDelay), now.Add(reconcile.TaskLifetime))
}
