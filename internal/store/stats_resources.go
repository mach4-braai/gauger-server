package store

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ways the Resources page groups jobs.
const (
	ByLabel    = "label"
	ByWorkflow = "workflow"
)

const resourcesMetricAvailable = "system.linux.memory.available"

// resourcesTopJobs is how many jobs each top list holds.
const resourcesTopJobs = 10

// resourcesUncounted are the interfaces whose traffic stays on the runner,
// the ones CountedInterface rejects.
var resourcesUncounted = []string{"lo", "tailscale0"}

// ResourceMode is one CPU mode's share of all CPUs over a group's samples.
// Mode is empty for the total series, which has no mode attribute.
type ResourceMode struct {
	Mode    string
	Samples int64
	Mean    float64
	Peak    float64
}

// ResourceState is one memory state's size in bytes over a group's samples.
type ResourceState struct {
	State string
	Mean  float64
	Peak  float64
}

// ResourceGroup is what the jobs of one runner label set, or of one
// workflow, used. Jobs counts those with samples in the window.
type ResourceGroup struct {
	Key    string
	Jobs   int64
	Modes  []ResourceMode
	Total  *ResourceMode
	States []ResourceState
	// PeakShare is the largest job peak of used memory over that job's
	// limit; MeanShare averages each job's mean used over its limit.
	PeakShare, MeanShare *float64
	PeakUsed, MeanUsed   *float64
	Limit                *float64
	MinAvailable         *float64
}

// ResourceTraffic is the bytes a group moved through one device or
// interface: read and written, or received and sent.
type ResourceTraffic struct {
	Group string
	Name  string
	In    float64
	Out   float64
}

// ResourceJob is one job's traffic, for the top lists.
type ResourceJob struct {
	ID         int64
	Repository string
	Workflow   string
	Name       string
	In         float64
	Out        float64
}

// ResourceBucket is the mean CPU use and memory use of every sample in one
// UTC bucket. Memory is mean used over mean limit. Each is nil when the
// bucket has none.
type ResourceBucket struct {
	Bucket time.Time
	CPU    *float64
	Memory *float64
}

// Resources compares what jobs used in a window.
type Resources struct {
	// Jobs are the jobs in the window, Sampled those with samples in it,
	// and FromArtifact those whose samples came from the fallback artifact.
	Jobs, Sampled, FromArtifact int64
	// Oldest is the oldest sample kept, nil when there are none.
	Oldest *time.Time
	// Since is where reading samples starts: the window's start, or the
	// start of retention if that is later.
	Since      time.Time
	Groups     []ResourceGroup
	Disk       []ResourceTraffic
	Network    []ResourceTraffic
	TopDisk    []ResourceJob
	TopNetwork []ResourceJob
	Timeline   []ResourceBucket
}

type stmt struct {
	sql  string
	args []any
}

func resourcesKey(by string) string {
	if by == ByWorkflow {
		return `j.repository || ' / ' || coalesce(nullif(j.workflow_name, ''), '(unnamed)')`
	}
	return `coalesce(nullif(array_to_string(j.labels, ', '), ''), '(none)')`
}

func resourcesSQL(by, sql string) string {
	return strings.ReplaceAll(sql, "{key}", resourcesKey(by))
}

// resourcesAttr is the regular expression that captures an attribute's
// value from a series string.
func resourcesAttr(key string) string {
	return regexp.QuoteMeta(key) + "=([^,]*)"
}

// Every query reads the samples with a time in [$5, $2): the later of the
// window's start and retention's, and the window's end. Bounding the
// partition key keeps Postgres to the daily partitions in range.

func resourcesCoverage(f Filter, lo time.Time) stmt {
	return stmt{`
		WITH` + windowed + `
		SELECT count(*),
			count(*) FILTER (WHERE EXISTS (SELECT 1 FROM samples m WHERE m.job_id = fj.id AND m.ts >= $5 AND m.ts < $2)),
			count(*) FILTER (WHERE fj.artifact_ingested_at IS NOT NULL
				AND EXISTS (SELECT 1 FROM samples m WHERE m.job_id = fj.id AND m.ts >= $5 AND m.ts < $2))
		FROM fj`, f.args(lo)}
}

func resourcesCPU(f Filter, lo time.Time, by string) stmt {
	return stmt{resourcesSQL(by, `
		WITH`+windowed+`
		SELECT {key}, CASE WHEN m.series = '' THEN '' ELSE substring(m.series FROM $7::text) END,
			count(DISTINCT m.job_id), count(*), avg(m.value), max(m.value)
		FROM samples m JOIN fj j ON j.id = m.job_id
		WHERE m.ts >= $5 AND m.ts < $2 AND m.metric = $6 AND (m.series = '' OR m.series ~ $7::text)
		GROUP BY 1, 2`),
		f.args(lo, MetricCPUUtilization, resourcesAttr(AttrCPUMode))}
}

func resourcesStates(f Filter, lo time.Time, by string) stmt {
	return stmt{resourcesSQL(by, `
		WITH`+windowed+`
		SELECT {key}, substring(m.series FROM $7::text), avg(m.value), max(m.value)
		FROM samples m JOIN fj j ON j.id = m.job_id
		WHERE m.ts >= $5 AND m.ts < $2 AND m.metric = $6 AND m.series ~ $7::text
		GROUP BY 1, 2`),
		f.args(lo, MetricMemoryUsage, resourcesAttr(AttrMemoryState))}
}

func resourcesHeadroom(f Filter, lo time.Time, by string) stmt {
	return stmt{resourcesSQL(by, `
		WITH`+windowed+`,
		pj AS (
			SELECT j.id, {key} AS grp,
				max(m.value) FILTER (WHERE m.metric = $6 AND m.series = $7) AS peak_used,
				avg(m.value) FILTER (WHERE m.metric = $6 AND m.series = $7) AS mean_used,
				max(m.value) FILTER (WHERE m.metric = $8) AS mem_limit,
				min(m.value) FILTER (WHERE m.metric = $9) AS min_available
			FROM samples m JOIN fj j ON j.id = m.job_id
			WHERE m.ts >= $5 AND m.ts < $2 AND m.metric = ANY($10::text[])
			GROUP BY 1, 2
		)
		SELECT grp, count(*), max(peak_used), avg(mean_used), max(mem_limit),
			max(peak_used / nullif(mem_limit, 0)), avg(mean_used / nullif(mem_limit, 0)), min(min_available)
		FROM pj
		GROUP BY 1`),
		f.args(lo, MetricMemoryUsage, SeriesMemoryUsed, MetricMemoryLimit, resourcesMetricAvailable,
			[]string{MetricMemoryUsage, MetricMemoryLimit, resourcesMetricAvailable})}
}

// resourcesTraffic starts the queries over a cumulative byte counter. It
// adds up each series' growth per job the way Increase does, a fall
// counting as a restart, and names the device or interface and direction
// from each series.
const resourcesTraffic = `
		WITH` + windowed + `,
		d AS (
			SELECT m.job_id, m.series, m.value,
				lag(m.value) OVER (PARTITION BY m.job_id, m.series ORDER BY m.ts) AS prev
			FROM samples m JOIN fj j ON j.id = m.job_id
			WHERE m.ts >= $5 AND m.ts < $2 AND m.metric = $6
		),
		named AS (
			SELECT job_id, substring(series FROM $7::text) AS name, substring(series FROM $8::text) AS dir,
				sum(CASE WHEN prev IS NULL THEN 0 WHEN value >= prev THEN value - prev ELSE value END) AS bytes
			FROM d
			WHERE series ~ $7::text AND NOT (substring(series FROM $7::text) = ANY($9::text[]))
			GROUP BY 1, 2, 3
		)`

// trafficKind is the counter a traffic query reads: its metric, the
// attributes that name the device and the direction, the two directions,
// and the names to leave out.
type trafficKind struct {
	metric, nameAttr, dirAttr string
	in, out                   string
	skip                      []string
}

var (
	diskTraffic    = trafficKind{MetricDiskIO, AttrDevice, AttrDiskDirection, "read", "write", []string{}}
	networkTraffic = trafficKind{MetricNetworkIO, AttrInterface, AttrNetworkDirection, "receive", "transmit", resourcesUncounted}
)

// args are the traffic queries' parameters from $6: the metric, the name
// and direction patterns, the names to leave out, and the two directions.
func (k trafficKind) args(f Filter, lo time.Time) []any {
	return f.args(lo, k.metric, resourcesAttr(k.nameAttr), resourcesAttr(k.dirAttr), k.skip, k.in, k.out)
}

func resourcesGroupTraffic(f Filter, lo time.Time, by string, k trafficKind) stmt {
	return stmt{resourcesSQL(by, resourcesTraffic+`
		SELECT {key}, n.name,
			coalesce(sum(n.bytes) FILTER (WHERE n.dir = $10), 0), coalesce(sum(n.bytes) FILTER (WHERE n.dir = $11), 0)
		FROM named n JOIN fj j ON j.id = n.job_id
		GROUP BY 1, 2
		ORDER BY 1, 2`), k.args(f, lo)}
}

func resourcesTopTraffic(f Filter, lo time.Time, k trafficKind) stmt {
	return stmt{resourcesTraffic + `
		SELECT j.id, j.repository, coalesce(j.workflow_name, ''), coalesce(j.name, ''),
			coalesce(sum(n.bytes) FILTER (WHERE n.dir = $10), 0), coalesce(sum(n.bytes) FILTER (WHERE n.dir = $11), 0)
		FROM named n JOIN fj j ON j.id = n.job_id
		GROUP BY j.id, j.repository, j.workflow_name, j.name
		HAVING coalesce(sum(n.bytes) FILTER (WHERE n.dir IN ($10, $11)), 0) > 0
		ORDER BY coalesce(sum(n.bytes) FILTER (WHERE n.dir IN ($10, $11)), 0) DESC, j.id
		LIMIT ` + strconv.Itoa(resourcesTopJobs), k.args(f, lo)}
}

func resourcesTimeline(f Filter, lo time.Time, step string) stmt {
	return stmt{`
		WITH` + windowed + `
		SELECT date_trunc($7, m.ts AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
			avg(m.value) FILTER (WHERE m.metric = $8 AND m.series = ''),
			avg(m.value) FILTER (WHERE m.metric = $9 AND m.series = $10),
			avg(m.value) FILTER (WHERE m.metric = $11)
		FROM samples m JOIN fj j ON j.id = m.job_id
		WHERE m.ts >= $5 AND m.ts < $2 AND m.metric = ANY($6::text[])
		GROUP BY 1
		ORDER BY 1`,
		f.args(lo, []string{MetricCPUUtilization, MetricMemoryUsage, MetricMemoryLimit}, step,
			MetricCPUUtilization, MetricMemoryUsage, SeriesMemoryUsed, MetricMemoryLimit)}
}

// resourcesStatements lists every query that reads samples.
func resourcesStatements(f Filter, lo time.Time, by, step string) []stmt {
	return []stmt{
		resourcesCoverage(f, lo),
		resourcesCPU(f, lo, by),
		resourcesStates(f, lo, by),
		resourcesHeadroom(f, lo, by),
		resourcesGroupTraffic(f, lo, by, diskTraffic),
		resourcesTopTraffic(f, lo, diskTraffic),
		resourcesGroupTraffic(f, lo, by, networkTraffic),
		resourcesTopTraffic(f, lo, networkTraffic),
		resourcesTimeline(f, lo, step),
	}
}

func resourcesRows[T any](ctx context.Context, s *Store, q stmt, scan pgx.RowToFunc[T]) ([]T, error) {
	rows, err := s.Pool.Query(ctx, q.sql, q.args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}

// Resources compares the runner use of the jobs in f's window, grouped by
// ByLabel or ByWorkflow. Only samples taken in the window, and no earlier
// than retention keeps them, count. step, "hour" or "day", sizes the
// timeline's buckets.
func (s *Store) Resources(ctx context.Context, f Filter, by, step string) (*Resources, error) {
	kept := s.RetentionStart(f.Until)
	lo := f.Since
	if lo.Before(kept) {
		lo = kept
	}
	r := &Resources{Since: lo}
	c := resourcesCoverage(f, lo)
	if err := s.Pool.QueryRow(ctx, c.sql, c.args...).Scan(&r.Jobs, &r.Sampled, &r.FromArtifact); err != nil {
		return nil, err
	}
	var err error
	if r.Oldest, err = s.oldestSample(ctx, kept, f.Until); err != nil {
		return nil, err
	}
	if r.Sampled == 0 {
		return r, nil
	}

	groups := map[string]*ResourceGroup{}
	group := func(key string) *ResourceGroup {
		g := groups[key]
		if g == nil {
			g = &ResourceGroup{Key: key}
			groups[key] = g
		}
		return g
	}

	type modeRow struct {
		key  string
		jobs int64
		mode ResourceMode
	}
	modes, err := resourcesRows(ctx, s, resourcesCPU(f, lo, by), func(row pgx.CollectableRow) (m modeRow, err error) {
		err = row.Scan(&m.key, &m.mode.Mode, &m.jobs, &m.mode.Samples, &m.mode.Mean, &m.mode.Peak)
		return
	})
	if err != nil {
		return nil, err
	}
	for _, m := range modes {
		g := group(m.key)
		g.Jobs = max(g.Jobs, m.jobs)
		mode := m.mode
		if mode.Mode == "" {
			g.Total = &mode
		} else {
			g.Modes = append(g.Modes, mode)
		}
	}

	type stateRow struct {
		key   string
		state ResourceState
	}
	states, err := resourcesRows(ctx, s, resourcesStates(f, lo, by), func(row pgx.CollectableRow) (st stateRow, err error) {
		err = row.Scan(&st.key, &st.state.State, &st.state.Mean, &st.state.Peak)
		return
	})
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		g := group(st.key)
		g.States = append(g.States, st.state)
	}

	type headroomRow struct {
		key  string
		jobs int64
		g    ResourceGroup
	}
	heads, err := resourcesRows(ctx, s, resourcesHeadroom(f, lo, by), func(row pgx.CollectableRow) (h headroomRow, err error) {
		err = row.Scan(&h.key, &h.jobs, &h.g.PeakUsed, &h.g.MeanUsed, &h.g.Limit, &h.g.PeakShare, &h.g.MeanShare, &h.g.MinAvailable)
		return
	})
	if err != nil {
		return nil, err
	}
	for _, h := range heads {
		g := group(h.key)
		g.Jobs = max(g.Jobs, h.jobs)
		g.PeakUsed, g.MeanUsed, g.Limit = h.g.PeakUsed, h.g.MeanUsed, h.g.Limit
		g.PeakShare, g.MeanShare, g.MinAvailable = h.g.PeakShare, h.g.MeanShare, h.g.MinAvailable
	}

	for _, g := range groups {
		slices.SortFunc(g.Modes, func(a, b ResourceMode) int { return strings.Compare(a.Mode, b.Mode) })
		slices.SortFunc(g.States, func(a, b ResourceState) int { return strings.Compare(a.State, b.State) })
		r.Groups = append(r.Groups, *g)
	}
	slices.SortFunc(r.Groups, func(a, b ResourceGroup) int {
		return cmp.Or(cmp.Compare(b.Jobs, a.Jobs), strings.Compare(a.Key, b.Key))
	})

	scanTraffic := func(row pgx.CollectableRow) (t ResourceTraffic, err error) {
		err = row.Scan(&t.Group, &t.Name, &t.In, &t.Out)
		return
	}
	scanJob := func(row pgx.CollectableRow) (j ResourceJob, err error) {
		err = row.Scan(&j.ID, &j.Repository, &j.Workflow, &j.Name, &j.In, &j.Out)
		return
	}
	if r.Disk, err = resourcesRows(ctx, s, resourcesGroupTraffic(f, lo, by, diskTraffic), scanTraffic); err != nil {
		return nil, err
	}
	if r.TopDisk, err = resourcesRows(ctx, s, resourcesTopTraffic(f, lo, diskTraffic), scanJob); err != nil {
		return nil, err
	}
	if r.Network, err = resourcesRows(ctx, s, resourcesGroupTraffic(f, lo, by, networkTraffic), scanTraffic); err != nil {
		return nil, err
	}
	if r.TopNetwork, err = resourcesRows(ctx, s, resourcesTopTraffic(f, lo, networkTraffic), scanJob); err != nil {
		return nil, err
	}

	type bucketRow struct {
		ResourceBucket
		used, limit *float64
	}
	buckets, err := resourcesRows(ctx, s, resourcesTimeline(f, lo, step), func(row pgx.CollectableRow) (b bucketRow, err error) {
		err = row.Scan(&b.Bucket, &b.CPU, &b.used, &b.limit)
		return
	})
	if err != nil {
		return nil, err
	}
	for _, b := range buckets {
		b.Bucket = b.Bucket.UTC()
		if b.used != nil && b.limit != nil && *b.limit > 0 {
			share := *b.used / *b.limit
			b.Memory = &share
		}
		r.Timeline = append(r.Timeline, b.ResourceBucket)
	}
	return r, nil
}

// oldestSample is the earliest sample time in [from, until), read one
// daily partition at a time from the oldest, so a database holding ninety
// days of samples reads one day.
func (s *Store) oldestSample(ctx context.Context, from, until time.Time) (*time.Time, error) {
	names, err := resourcesRows(ctx, s, stmt{`
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'samples' ORDER BY 1`, nil}, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		day, err := time.Parse("20060102", strings.TrimPrefix(name, partitionPrefix))
		if err != nil || day.Before(dayStart(from)) || !day.Before(until) {
			continue
		}
		start, end := day, day.AddDate(0, 0, 1)
		if from.After(start) {
			start = from
		}
		if until.Before(end) {
			end = until
		}
		var oldest *time.Time
		err = s.Pool.QueryRow(ctx, `SELECT min(ts) FROM samples WHERE ts >= $1 AND ts < $2`, start, end).Scan(&oldest)
		if err != nil {
			return nil, err
		}
		if oldest != nil {
			t := oldest.UTC()
			return &t, nil
		}
	}
	return nil, nil
}
