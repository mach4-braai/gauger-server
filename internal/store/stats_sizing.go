package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// SizingJob is one job with runner samples and how much of its runner it
// used. Memory is in bytes and CPU a fraction of all cores; each is nil
// when gauger did not report the value or its total. Minutes is zero until
// the job completes.
type SizingJob struct {
	ID         int64
	Repository string
	Workflow   string
	Path       string
	Name       string
	Labels     []string
	Private    *bool
	Minutes    int64
	PeakMemory *float64
	MemTotal   *float64
	PeakCPU    *float64
	P95CPU     *float64
	CPUCount   *float64
}

// SizingJobs returns every job in the window that has runner samples,
// with its peak memory used, its peak and 95th percentile CPU use, and
// the runner's MemTotal and core count. It also returns how many jobs the
// window holds, sampled or not.
func (s *Store) SizingJobs(ctx context.Context, f Filter) ([]SizingJob, int64, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT fj.id, fj.repository, coalesce(fj.workflow_name, ''), coalesce(r.path, ''), coalesce(fj.name, ''),
			fj.labels, rp.private,
			CASE WHEN fj.status = 'completed' AND fj.started_at IS NOT NULL AND fj.completed_at > fj.started_at
				THEN ceil(extract(epoch FROM fj.completed_at - fj.started_at) / 60)::bigint ELSE 0 END,
			m.peak_mem, m.mem_total, m.peak_cpu, m.p95_cpu, m.cpus
		FROM fj
		LEFT JOIN runs r ON r.id = fj.run_id AND r.attempt = fj.run_attempt
		LEFT JOIN repositories rp ON rp.full_name = fj.repository
		CROSS JOIN LATERAL (
			SELECT max(value) FILTER (WHERE metric = $5 AND series = $6) AS peak_mem,
				max(value) FILTER (WHERE metric = $7) AS mem_total,
				max(value) FILTER (WHERE metric = $8 AND series = '') AS peak_cpu,
				percentile_cont(0.95) WITHIN GROUP (ORDER BY value) FILTER (WHERE metric = $8 AND series = '') AS p95_cpu,
				max(value) FILTER (WHERE metric = $9) AS cpus
			FROM samples WHERE job_id = fj.id AND metric IN ($5, $7, $8, $9)
		) m
		WHERE EXISTS (SELECT 1 FROM samples x WHERE x.job_id = fj.id)
		ORDER BY fj.id`,
		f.args(MetricMemoryUsage, SeriesMemoryUsed, MetricMemoryLimit, MetricCPUUtilization, MetricCPUCount)...)
	if err != nil {
		return nil, 0, err
	}
	jobs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[SizingJob])
	if err != nil {
		return nil, 0, err
	}
	var total int64
	err = s.Pool.QueryRow(ctx, `WITH`+windowed+` SELECT count(*) FROM fj`, f.args()...).Scan(&total)
	return jobs, total, err
}
