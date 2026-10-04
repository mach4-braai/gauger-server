package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// BreakdownDimension is what Breakdown groups a window by.
type BreakdownDimension string

const (
	BreakdownByRepository BreakdownDimension = "repo"
	BreakdownByWorkflow   BreakdownDimension = "workflow"
	BreakdownByJob        BreakdownDimension = "job"
	BreakdownByEvent      BreakdownDimension = "event"
	BreakdownByBranch     BreakdownDimension = "branch"
)

// BreakdownRow is one group of a Breakdown. Repository is set when
// grouping by repository, workflow or job. Path and Workflow are the
// workflow file and its label, set when grouping by workflow or job, so a
// dynamic workflow is one group however many pull requests ran it. Name is
// the group's own value: the repository, workflow label, job name, event
// or branch.
//
// Runs counts run attempts. When grouping by job a run counts once under
// every job it ran, and Decided and Succeeded count jobs, so the job's own
// outcome and duration are what the row reports. Otherwise they count
// runs: a run is decided when it completed with a conclusion other than
// cancelled or skipped. Durations are in seconds and nil when the group
// has none. QueueP95 is over jobs: a job's start minus its creation.
type BreakdownRow struct {
	Repository string
	Path       string
	Workflow   string
	Name       string
	Runs       int64
	Decided    int64
	Succeeded  int64
	Jobs       int64
	P50        *float64
	P95        *float64
	QueueP95   *float64
	// Minutes is the group's job minutes per runner label set, each job
	// rounded up to a whole minute, for pricing.
	Minutes []LabelMinutes
	// Spark is the group's runs per UTC bucket, for the buckets that have
	// any.
	Spark []BucketCount
}

// BucketCount is a number of runs started in one bucket.
type BucketCount struct {
	Bucket time.Time
	Runs   int64
}

// breakdownKeys are the SQL expressions that name a group, over the
// windowed jobs fj, their run r and their workflow jw, and over the
// windowed runs fr with its workflow wf.
type breakdownKeys struct {
	job, run string
	// needWorkflow joins jw and wf, which the group's path and label come from.
	needWorkflow bool
}

var breakdownDimensions = map[BreakdownDimension]breakdownKeys{
	BreakdownByRepository: {
		job: `fj.repository AS repository, '' AS path, '' AS workflow, fj.repository AS name`,
		run: `fr.repository AS repository, '' AS path, '' AS workflow, fr.repository AS name`,
	},
	BreakdownByWorkflow: {
		job:          `fj.repository AS repository, jw.path AS path, jw.workflow AS workflow, jw.workflow AS name`,
		run:          `fr.repository AS repository, coalesce(fr.path, wp.path, '') AS path, coalesce(wf.name, fr.workflow_name, '') AS workflow, coalesce(wf.name, fr.workflow_name, '') AS name`,
		needWorkflow: true,
	},
	BreakdownByJob: {
		job:          `fj.repository AS repository, jw.path AS path, jw.workflow AS workflow, coalesce(fj.name, '') AS name`,
		needWorkflow: true,
	},
	BreakdownByEvent: {
		job: `'' AS repository, '' AS path, '' AS workflow, coalesce(r.event, '') AS name`,
		run: `'' AS repository, '' AS path, '' AS workflow, coalesce(fr.event, '') AS name`,
	},
	BreakdownByBranch: {
		job: `'' AS repository, '' AS path, '' AS workflow, coalesce(fj.head_branch, r.head_branch, '') AS name`,
		run: `'' AS repository, '' AS path, '' AS workflow, coalesce(fr.head_branch, '') AS name`,
	},
}

const (
	decidedSQL   = `status = 'completed' AND coalesce(conclusion, '') NOT IN ('', 'cancelled', 'skipped')`
	succeededSQL = `status = 'completed' AND conclusion = 'success'`
)

// breakdownCTEs defines jk, the windowed jobs with their group, and rk,
// the rows that count runs: one per windowed run, or per windowed job when
// grouping by job. Both carry the group as repository, path, workflow and
// name. A query using it starts with "WITH" and takes Filter.args.
func breakdownCTEs(by BreakdownDimension) (string, error) {
	k, ok := breakdownDimensions[by]
	if !ok {
		return "", fmt.Errorf("unknown breakdown dimension %q", by)
	}
	jobWorkflow := ""
	if k.needWorkflow {
		jobWorkflow = "JOIN jw ON jw.id = fj.id"
	}
	q := `WITH` + windowed + `,` + jobWorkflows + `,
		jk AS (
			SELECT fj.id, ` + k.job + `, fj.run_id, fj.run_attempt, fj.status, fj.conclusion,
				fj.created_at, fj.started_at, fj.completed_at, fj.labels, rp.private,
				coalesce(fj.started_at, fj.created_at, fj.runner_seen_at) AS ts
			FROM fj
			LEFT JOIN runs r ON r.id = fj.run_id AND r.attempt = fj.run_attempt
			` + jobWorkflow + `
			LEFT JOIN repositories rp ON rp.full_name = fj.repository
		),`
	if by == BreakdownByJob {
		return q + `
		rk AS (
			SELECT run_id AS id, run_attempt AS attempt, repository, path, workflow, name, ts,
				` + decidedSQL + ` AS decided, ` + succeededSQL + ` AS succeeded,
				CASE WHEN status = 'completed' AND started_at IS NOT NULL AND completed_at IS NOT NULL
					THEN extract(epoch FROM completed_at - started_at) END AS secs
			FROM jk
		)`, nil
	}
	runWorkflow := ""
	if k.needWorkflow {
		runWorkflow = `LEFT JOIN workflow_path wp ON wp.repository = fr.repository AND wp.workflow = fr.workflow_name
			LEFT JOIN wf ON wf.repository = fr.repository AND wf.path = coalesce(fr.path, wp.path)`
	}
	return q + `
		rk AS (
			SELECT fr.id, fr.attempt, ` + k.run + `, coalesce(fr.run_started_at, fr.created_at) AS ts,
				fr.status = 'completed' AND coalesce(fr.conclusion, '') NOT IN ('', 'cancelled', 'skipped') AS decided,
				fr.status = 'completed' AND fr.conclusion = 'success' AS succeeded,
				d.secs
			FROM fr
			` + runWorkflow + `
			LEFT JOIN LATERAL (
				SELECT extract(epoch FROM max(j.completed_at) - min(j.started_at)) AS secs
				FROM jobs j
				WHERE fr.status = 'completed' AND j.run_id = fr.id AND j.run_attempt = fr.attempt
				  AND j.started_at IS NOT NULL AND j.completed_at IS NOT NULL
			) d ON true
		)`, nil
}

// runCount counts the runs of a group of rk.
func runCount(by BreakdownDimension) string {
	if by == BreakdownByJob {
		return `count(DISTINCT (id, attempt)) FILTER (WHERE id IS NOT NULL)`
	}
	return `count(*)`
}

type breakdownKey struct{ repository, path, workflow, name string }

// Breakdown groups the window by one dimension. A run counts under its own
// repository, workflow file, event and branch, and a job under its own, so
// the groups add up to the Overview's runs, jobs and minutes. Spark buckets
// are "hour" or "day". Rows come back ordered by group.
func (s *Store) Breakdown(ctx context.Context, f Filter, by BreakdownDimension, bucket string) ([]BreakdownRow, error) {
	ctes, err := breakdownCTEs(by)
	if err != nil {
		return nil, err
	}
	runs := runCount(by)
	rows, err := s.Pool.Query(ctx, ctes+`,
		ra AS (
			SELECT repository, path, workflow, name, `+runs+` AS runs,
				count(*) FILTER (WHERE decided) AS decided, count(*) FILTER (WHERE succeeded) AS succeeded,
				percentile_cont(0.5) WITHIN GROUP (ORDER BY secs) AS p50,
				percentile_cont(0.95) WITHIN GROUP (ORDER BY secs) AS p95
			FROM rk
			GROUP BY 1, 2, 3, 4
		),
		ja AS (
			SELECT repository, path, workflow, name, count(*) AS jobs,
				percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - created_at))
					FILTER (WHERE started_at >= created_at) AS queue_p95
			FROM jk
			GROUP BY 1, 2, 3, 4
		)
		SELECT repository, path, workflow, name, coalesce(runs, 0), coalesce(decided, 0), coalesce(succeeded, 0),
			coalesce(jobs, 0), p50, p95, queue_p95
		FROM ra FULL JOIN ja USING (repository, path, workflow, name)
		ORDER BY 1, 2, 3, 4`, f.args()...)
	if err != nil {
		return nil, err
	}
	groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BreakdownRow, error) {
		var g BreakdownRow
		err := row.Scan(&g.Repository, &g.Path, &g.Workflow, &g.Name, &g.Runs, &g.Decided, &g.Succeeded,
			&g.Jobs, &g.P50, &g.P95, &g.QueueP95)
		return g, err
	})
	if err != nil {
		return nil, err
	}
	index := make(map[breakdownKey]*BreakdownRow, len(groups))
	for i := range groups {
		g := &groups[i]
		index[breakdownKey{g.Repository, g.Path, g.Workflow, g.Name}] = g
	}

	rows, err = s.Pool.Query(ctx, ctes+`
		SELECT repository, path, workflow, name, labels, private,
			sum(ceil(extract(epoch FROM completed_at - started_at) / 60))::bigint
		FROM jk
		WHERE status = 'completed' AND started_at IS NOT NULL AND completed_at > started_at
		GROUP BY 1, 2, 3, 4, 5, 6
		ORDER BY 1, 2, 3, 4, 7 DESC`, f.args()...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k breakdownKey
		var m LabelMinutes
		if err := rows.Scan(&k.repository, &k.path, &k.workflow, &k.name, &m.Labels, &m.Private, &m.Minutes); err != nil {
			rows.Close()
			return nil, err
		}
		g := index[k]
		g.Minutes = append(g.Minutes, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.Pool.Query(ctx, ctes+`
		SELECT repository, path, workflow, name,
			date_trunc($5, ts AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', `+runs+`
		FROM rk
		GROUP BY 1, 2, 3, 4, 5
		ORDER BY 1, 2, 3, 4, 5`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k breakdownKey
		var c BucketCount
		if err := rows.Scan(&k.repository, &k.path, &k.workflow, &k.name, &c.Bucket, &c.Runs); err != nil {
			rows.Close()
			return nil, err
		}
		g := index[k]
		g.Spark = append(g.Spark, c)
	}
	return groups, rows.Err()
}
