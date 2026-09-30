package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/webhook"
)

const secret = "hook-secret"

func newHandler(t *testing.T) (*webhook.Handler, *store.Store) {
	st := storetest.Open(t, 90*24*time.Hour)
	return &webhook.Handler{Store: st, Creds: github.StaticCredentials{C: &github.Credentials{WebhookSecret: secret}}}, st
}

func post(t *testing.T, h http.Handler, event, delivery string, body []byte, sig string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func ts(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

func jobEvent(t *testing.T, status, conclusion string, steps []github.Step) []byte {
	var completedAt *time.Time
	if status == "completed" {
		completedAt = ts("2026-09-30T10:05:00Z")
	}
	b, err := json.Marshal(github.WorkflowJobEvent{
		Action: status,
		WorkflowJob: github.Job{
			ID: 555, RunID: 100, RunAttempt: 1, Name: "build", WorkflowName: "CI", HeadBranch: "main",
			Status: status, Conclusion: conclusion, Labels: []string{"ubuntu-latest"}, RunnerName: "GitHub Actions 3",
			StartedAt: ts("2026-09-30T10:00:00Z"), CompletedAt: completedAt,
			Steps: steps,
		},
		Repository:   github.Repository{FullName: "acme/app", Private: new(true)},
		Installation: github.Installation{ID: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignatureIsCheckedBeforeParsing(t *testing.T) {
	h, _ := newHandler(t)
	body := []byte("not json")
	if code := post(t, h, "workflow_job", "d1", body, github.Sign("wrong", body)); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status %d, want 401", code)
	}
	if code := post(t, h, "workflow_job", "d1", body, ""); code != http.StatusUnauthorized {
		t.Fatalf("missing signature: status %d, want 401", code)
	}
	if code := post(t, h, "workflow_job", "d1", body, github.Sign(secret, body)); code != http.StatusBadRequest {
		t.Fatalf("signed garbage: status %d, want 400", code)
	}
}

func TestJobEventsNeverMoveBackwards(t *testing.T) {
	h, st := newHandler(t)
	ctx := context.Background()
	steps := []github.Step{
		{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: ts("2026-09-30T10:00:00Z"), CompletedAt: ts("2026-09-30T10:00:02Z")},
		{Number: 2, Name: "Run make", Status: "completed", Conclusion: "success", StartedAt: ts("2026-09-30T10:00:02Z"), CompletedAt: ts("2026-09-30T10:04:58Z")},
	}
	completed := jobEvent(t, "completed", "success", steps)
	if code := post(t, h, "workflow_job", "d-completed", completed, github.Sign(secret, completed)); code != http.StatusNoContent {
		t.Fatalf("completed: status %d", code)
	}
	if code := post(t, h, "workflow_job", "d-completed", completed, github.Sign(secret, completed)); code != http.StatusOK {
		t.Fatalf("redelivery: status %d, want 200", code)
	}
	late := jobEvent(t, "in_progress", "", []github.Step{{Number: 1, Name: "Set up job", Status: "in_progress"}})
	if code := post(t, h, "workflow_job", "d-late", late, github.Sign(secret, late)); code != http.StatusNoContent {
		t.Fatalf("late in_progress: status %d", code)
	}

	var status, conclusion string
	var completedAt *time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT status, conclusion, completed_at FROM jobs WHERE id = 555`).Scan(&status, &conclusion, &completedAt); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || conclusion != "success" || completedAt == nil {
		t.Fatalf("job = %s/%s completed_at=%v, want completed/success with a completion time", status, conclusion, completedAt)
	}
	var stepStatus string
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*), min(status) FROM steps WHERE job_id = 555`).Scan(&n, &stepStatus); err != nil {
		t.Fatal(err)
	}
	if n != 2 || stepStatus != "completed" {
		t.Fatalf("steps = %d with min status %q, want 2 completed", n, stepStatus)
	}
	var deliveries int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries`).Scan(&deliveries)
	if deliveries != 2 {
		t.Fatalf("deliveries = %d, want 2", deliveries)
	}
}

func TestRunEventsScheduleRepair(t *testing.T) {
	h, st := newHandler(t)
	ctx := context.Background()
	run := func(status string) []byte {
		b, _ := json.Marshal(github.WorkflowRunEvent{
			Action:       status,
			WorkflowRun:  github.Run{ID: 100, RunAttempt: 1, Name: "CI", Status: status},
			Repository:   github.Repository{FullName: "acme/app"},
			Installation: github.Installation{ID: 9},
		})
		return b
	}
	nextAt := func() time.Duration {
		var next time.Time
		if err := st.Pool.QueryRow(ctx, `SELECT next_at FROM tasks WHERE kind = 'run' AND key = '100:1'`).Scan(&next); err != nil {
			t.Fatal(err)
		}
		return time.Until(next)
	}

	b := run("in_progress")
	post(t, h, "workflow_run", "r1", b, github.Sign(secret, b))
	if d := nextAt(); d < 9*time.Minute || d > 10*time.Minute {
		t.Fatalf("in-progress run check in %v, want about 10m", d)
	}
	b = run("completed")
	post(t, h, "workflow_run", "r2", b, github.Sign(secret, b))
	if d := nextAt(); d > time.Minute {
		t.Fatalf("completed run check in %v, want within 1m", d)
	}
	var inst int64
	st.Pool.QueryRow(ctx, `SELECT installation_id FROM repositories WHERE full_name = 'acme/app'`).Scan(&inst)
	if inst != 9 {
		t.Fatalf("installation = %d, want 9", inst)
	}
}
