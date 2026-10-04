package store

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// LabelStat is one set of runner labels over a window. Minutes round each
// completed job up to a whole minute. Queue time is a job's start minus
// its creation. Billing splits Minutes by repository visibility, which
// decides whether a label is free.
type LabelStat struct {
	Labels   []string
	Jobs     int64
	Minutes  int64
	QueueP50 *float64
	QueueP95 *float64
	Billing  []LabelMinutes
}

// Name is the label set as one string: "ubuntu-latest" or "self-hosted,
// linux".
func (l LabelStat) Name() string { return LabelName(l.Labels) }

// LabelName joins a job's runner labels into one name.
func LabelName(labels []string) string {
	if len(labels) == 0 {
		return "(none)"
	}
	return strings.Join(labels, ", ")
}

// LabelStats returns every set of runner labels with jobs in the window,
// the one with the most minutes first.
func (s *Store) LabelStats(ctx context.Context, f Filter) ([]LabelStat, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`,
		q AS (
			SELECT labels,
				percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - created_at)) AS p50,
				percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - created_at)) AS p95
			FROM fj WHERE started_at >= created_at GROUP BY labels
		),
		u AS (
			SELECT fj.labels, rp.private, count(*) AS jobs,
				coalesce(sum(ceil(extract(epoch FROM fj.completed_at - fj.started_at) / 60))
					FILTER (WHERE fj.status = 'completed' AND fj.started_at IS NOT NULL AND fj.completed_at > fj.started_at), 0)::bigint AS minutes
			FROM fj LEFT JOIN repositories rp ON rp.full_name = fj.repository
			GROUP BY 1, 2
		)
		SELECT u.labels, u.private, u.jobs, u.minutes, q.p50, q.p95 FROM u LEFT JOIN q ON q.labels = u.labels`, f.args()...)
	if err != nil {
		return nil, err
	}
	type row struct {
		Labels   []string
		Private  *bool
		Jobs     int64
		Minutes  int64
		P50, P95 *float64
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return nil, err
	}
	var out []LabelStat
	index := map[string]int{}
	for _, r := range got {
		name := LabelName(r.Labels)
		i, ok := index[name]
		if !ok {
			i = len(out)
			index[name] = i
			out = append(out, LabelStat{Labels: r.Labels, QueueP50: r.P50, QueueP95: r.P95})
		}
		out[i].Jobs += r.Jobs
		out[i].Minutes += r.Minutes
		if r.Minutes > 0 {
			out[i].Billing = append(out[i].Billing, LabelMinutes{Labels: r.Labels, Private: r.Private, Minutes: r.Minutes})
		}
	}
	slices.SortFunc(out, func(a, b LabelStat) int {
		return cmp.Or(cmp.Compare(b.Minutes, a.Minutes), cmp.Compare(a.Name(), b.Name()))
	})
	return out, nil
}

// LabelBucketMinutes is the job minutes of jobs on one set of labels that
// started in one bucket.
type LabelBucketMinutes struct {
	Bucket  time.Time
	Labels  []string
	Minutes int64
}

// LabelMinutesByBucket sums job minutes per UTC bucket ("hour" or "day")
// and runner labels.
func (s *Store) LabelMinutesByBucket(ctx context.Context, f Filter, bucket string) ([]LabelBucketMinutes, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT date_trunc($5, started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', labels,
			sum(ceil(extract(epoch FROM completed_at - started_at) / 60))::bigint
		FROM fj
		WHERE status = 'completed' AND started_at IS NOT NULL AND completed_at > started_at
		GROUP BY 1, 2
		ORDER BY 1, 2`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LabelBucketMinutes])
}

// BucketLoad is the queue time of jobs that started in one bucket, and
// the most jobs running at once during it. The queue figures are nil when
// no job started in the bucket.
type BucketLoad struct {
	Bucket   time.Time
	QueueP50 *float64
	QueueP95 *float64
	Peak     int64
}

// LoadByBucket returns queue p50 and p95 and peak concurrency per UTC
// bucket ("hour" or "day"), for the buckets where either exists. A job
// runs from its start to its completion; one that completes the instant
// another starts does not overlap it. Jobs still running have no end and
// are left out of the peak.
func (s *Store) LoadByBucket(ctx context.Context, f Filter, bucket string) ([]BucketLoad, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`,
		q AS (
			SELECT date_trunc($5, started_at AT TIME ZONE 'UTC') AS b,
				percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - created_at)) AS p50,
				percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - created_at)) AS p95
			FROM fj WHERE started_at >= created_at GROUP BY 1
		),
		ev AS (
			SELECT started_at AS t, 1 AS d FROM fj WHERE completed_at > started_at
			UNION ALL
			SELECT completed_at, -1 FROM fj WHERE completed_at > started_at
		),
		run AS (
			SELECT t, d, sum(d) OVER (ORDER BY t, d) AS n FROM ev
		),
		bk AS (
			SELECT date_trunc($5, t AT TIME ZONE 'UTC') AS b, max(n) AS peak,
				(array_agg(n ORDER BY t DESC, d DESC))[1] AS last_n
			FROM run GROUP BY 1
		),
		ab AS (
			SELECT generate_series((SELECT min(b) FROM bk), (SELECT max(b) FROM bk), ('1 ' || $5)::interval) AS b
			UNION
			SELECT b FROM q
		)
		SELECT ab.b AT TIME ZONE 'UTC', q.p50, q.p95,
			greatest(coalesce(bk.peak, 0), coalesce((SELECT p.last_n FROM bk p WHERE p.b < ab.b ORDER BY p.b DESC LIMIT 1), 0))::bigint
		FROM ab
		LEFT JOIN q ON q.b = ab.b
		LEFT JOIN bk ON bk.b = ab.b
		ORDER BY 1`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[BucketLoad])
}

// HourQueue is the queue p95 of jobs on one set of labels that started in
// one hour of the UTC day.
type HourQueue struct {
	Labels []string
	Hour   int
	P95    float64
}

// QueueByHour returns queue p95 per hour of the day and runner labels.
func (s *Store) QueueByHour(ctx context.Context, f Filter) ([]HourQueue, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT labels, extract(hour FROM started_at AT TIME ZONE 'UTC')::int,
			percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - created_at))
		FROM fj
		WHERE started_at >= created_at
		GROUP BY 1, 2
		ORDER BY 1, 2`, f.args()...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[HourQueue])
}

// StartDelay is how long runs wait before their first job starts, from a
// run attempt's creation to the earliest start of its jobs, in seconds.
// The figures are nil when no run in the window has a started job.
type StartDelay struct {
	P50 *float64
	P95 *float64
}

// RunStartDelay measures the time from a run's creation to its first job
// starting.
func (s *Store) RunStartDelay(ctx context.Context, f Filter) (StartDelay, error) {
	var d StartDelay
	err := s.Pool.QueryRow(ctx, `
		WITH`+windowed+`,
		d AS (
			SELECT extract(epoch FROM min(j.started_at) - fr.created_at) AS secs
			FROM fr JOIN jobs j ON j.run_id = fr.id AND j.run_attempt = fr.attempt
			WHERE j.started_at IS NOT NULL
			GROUP BY fr.id, fr.attempt, fr.created_at
			HAVING min(j.started_at) >= fr.created_at
		)
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY secs),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY secs)
		FROM d`, f.args()...).Scan(&d.P50, &d.P95)
	return d, err
}

// WeekdayHourCount is the number of runs that started in one hour of one
// UTC weekday. Weekday is 0 for Monday through 6 for Sunday.
type WeekdayHourCount struct {
	Weekday int
	Hour    int
	Runs    int64
}

// RunsByWeekdayHour counts run attempts per UTC weekday and hour.
func (s *Store) RunsByWeekdayHour(ctx context.Context, f Filter) ([]WeekdayHourCount, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT extract(isodow FROM t)::int - 1, extract(hour FROM t)::int, count(*)
		FROM (SELECT coalesce(run_started_at, created_at) AT TIME ZONE 'UTC' AS t FROM fr) x
		GROUP BY 1, 2
		ORDER BY 1, 2`, f.args()...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[WeekdayHourCount])
}
