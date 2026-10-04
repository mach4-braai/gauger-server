package store

import (
	"context"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// stepName is the SQL expression that normalises the step name in column
// col. It strips the "@<ref>" from action steps, so "Run jdx/mise-action@9e7f"
// and "Run jdx/mise-action@7e36" are one step. Queries that group steps by
// name use it.
func stepName(col string) string {
	return `regexp_replace(` + col + `, '^((?:Post )?(?:Run )?[^\s@/]+/[^\s@]+)@\S+$', '\1')`
}

// setupPatterns match normalised step names that prepare or tear down a
// job instead of doing its work.
var setupPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(Set up job|Complete job)$`),
	regexp.MustCompile(`(?i)^Post `),
	regexp.MustCompile(`(?i)^(Run )?actions/checkout$`),
	regexp.MustCompile(`(?i)^(Run )?actions/cache(/restore)?$`),
	regexp.MustCompile(`(?i)^(Run )?jdx/mise-action$`),
}

// IsSetupStep reports whether a normalised step name is setup, not work.
func IsSetupStep(name string) bool {
	for _, p := range setupPatterns {
		if p.MatchString(name) {
			return true
		}
	}
	return false
}

// ranSteps continues a windowed query with ran, the steps of the window's
// jobs that ran: both times set and a conclusion other than skipped.
// Their names are normalised and secs is their duration.
var ranSteps = `,
		ran AS (
			SELECT fj.id AS job_id, fj.html_url, s.number, s.started_at, s.conclusion,
				` + stepName("s.name") + ` AS name,
				extract(epoch FROM s.completed_at - s.started_at)::float8 AS secs
			FROM steps s JOIN fj ON fj.id = s.job_id
			WHERE s.started_at IS NOT NULL AND s.completed_at IS NOT NULL
			  AND s.conclusion IS DISTINCT FROM 'skipped'
		)`

// StepStat summarises one normalised step name over the steps that ran in
// the window. Seconds is the total time, P50 and P95 are per occurrence,
// and the Slowest fields locate the longest occurrence, ties going to the
// highest job and step number.
type StepStat struct {
	Name           string
	Setup          bool
	Runs           int64
	Seconds        float64
	P50            float64
	P95            float64
	Failures       int64
	SlowestSeconds float64
	JobID          int64
	StepNumber     int
	JobHTMLURL     string
}

// StepBucket is the time one step name took among the steps that started
// in one UTC bucket.
type StepBucket struct {
	Bucket  time.Time
	Name    string
	Setup   bool
	Seconds float64
}

// StepStats holds the steps page's data: one StepStat per name, most
// seconds first, and the seconds per name and bucket.
type StepStats struct {
	Steps   []StepStat
	Buckets []StepBucket
}

// Steps returns per-step statistics for the window, with time per UTC
// bucket ("hour" or "day") by step name.
func (s *Store) Steps(ctx context.Context, f Filter, bucket string) (*StepStats, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+ranSteps+`
		SELECT name, count(*), sum(secs),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY secs),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY secs),
			count(*) FILTER (WHERE conclusion = 'failure'),
			max(secs),
			(array_agg(job_id ORDER BY secs DESC, job_id DESC, number DESC))[1],
			(array_agg(number ORDER BY secs DESC, job_id DESC, number DESC))[1],
			coalesce((array_agg(html_url ORDER BY secs DESC, job_id DESC, number DESC))[1], '')
		FROM ran
		GROUP BY name
		ORDER BY sum(secs) DESC, name`, f.args()...)
	if err != nil {
		return nil, err
	}
	steps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (StepStat, error) {
		var st StepStat
		err := row.Scan(&st.Name, &st.Runs, &st.Seconds, &st.P50, &st.P95, &st.Failures,
			&st.SlowestSeconds, &st.JobID, &st.StepNumber, &st.JobHTMLURL)
		st.Setup = IsSetupStep(st.Name)
		return st, err
	})
	if err != nil {
		return nil, err
	}

	rows, err = s.Pool.Query(ctx, `
		WITH`+windowed+ranSteps+`
		SELECT date_trunc($5, started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', name, sum(secs)
		FROM ran
		GROUP BY 1, 2
		ORDER BY 1, 2`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	buckets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (StepBucket, error) {
		var b StepBucket
		err := row.Scan(&b.Bucket, &b.Name, &b.Seconds)
		b.Setup = IsSetupStep(b.Name)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	return &StepStats{Steps: steps, Buckets: buckets}, nil
}
