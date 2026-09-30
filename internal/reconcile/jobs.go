package reconcile

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/runner"
	"github.com/mach4-braai/gauger-server/internal/store"
)

// jobTask polls a job gauger reported on until GitHub says it completed,
// then stores its step timings.
func (r *Reconciler) jobTask(ctx context.Context, t store.Task) (time.Duration, error) {
	jobID, err := strconv.ParseInt(t.Key, 10, 64)
	if err != nil {
		return 0, nil
	}
	state, err := r.Store.JobState(ctx, jobID)
	if err != nil {
		return 0, err
	}
	if state == nil || state.Status == "completed" {
		return 0, nil
	}
	inst, err := r.Installation(ctx, t.Repository)
	if err != nil {
		return 0, err
	}
	job, err := r.GitHub.GetJob(ctx, inst, t.Repository, jobID)
	if err != nil {
		return 0, err
	}
	err = r.Store.InTx(ctx, func(tx pgx.Tx) error { return store.UpsertJob(ctx, tx, t.Repository, job) })
	if err != nil {
		return 0, err
	}
	if job.Status == "completed" {
		return 0, nil
	}
	return backoff(t.Attempts, 15*time.Second, 5*time.Minute), nil
}

// artifactTask looks for the fallback artifact of a job whose samples
// never finished arriving, until it shows up or the task expires.
func (r *Reconciler) artifactTask(ctx context.Context, t store.Task) (time.Duration, error) {
	jobID, err := strconv.ParseInt(t.Key, 10, 64)
	if err != nil {
		return 0, nil
	}
	state, err := r.Store.JobState(ctx, jobID)
	if err != nil {
		return 0, err
	}
	if state == nil || state.RunnerDoneAt != nil {
		return 0, nil
	}
	inst, err := r.Installation(ctx, t.Repository)
	if err != nil {
		return 0, err
	}
	name := runner.ArtifactName(jobID)
	arts, err := r.GitHub.ListRunArtifacts(ctx, inst, t.Repository, state.RunID, name)
	if err != nil {
		return 0, err
	}
	log := slog.With("job_id", jobID, "artifact", name)
	for _, a := range arts {
		if a.Name != name {
			continue
		}
		if a.Expired {
			log.Warn("fallback artifact expired before it was read")
			return 0, nil
		}
		zip, err := r.GitHub.DownloadArtifact(ctx, inst, t.Repository, a.ID)
		if err != nil {
			return 0, err
		}
		batches, err := runner.ReadArtifact(zip)
		if err != nil {
			log.Warn("fallback artifact is unreadable", "err", err)
			return 0, nil
		}
		var stored, rejected int64
		for _, b := range batches {
			points, bad := runner.Points(b)
			dropped, err := r.Store.InsertSamples(ctx, jobID, points)
			if err != nil {
				return 0, err
			}
			stored += int64(len(points)) - dropped
			rejected += bad + dropped
		}
		if err := r.Store.MarkArtifactIngested(ctx, jobID); err != nil {
			return 0, err
		}
		log.Info("ingested fallback artifact", "points", stored, "rejected", rejected)
		return 0, nil
	}
	return backoff(t.Attempts, 5*time.Minute, 6*time.Hour), nil
}
