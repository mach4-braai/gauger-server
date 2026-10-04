package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

var breakdownDimensions = []store.BreakdownDimension{
	store.BreakdownByRepository, store.BreakdownByWorkflow, store.BreakdownByJob,
	store.BreakdownByEvent, store.BreakdownByBranch,
}

func TestBreakdownAddsUpToOverview(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	until := fx.Now.Add(time.Minute)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		f    store.Filter
	}{
		{"all time", store.Filter{Until: until}},
		{"30 days", store.Filter{Since: until.Add(-30 * 24 * time.Hour), Until: until}},
		{"one repository", store.Filter{Repository: "acme/api", Until: until}},
		{"one event", store.Filter{Event: "pull_request", Until: until}},
	} {
		o, err := st.Overview(ctx, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		if o.Runs == 0 || o.Jobs == 0 {
			t.Fatalf("%s: fixtures have no runs or jobs; the case checks nothing", tc.name)
		}
		var wantMinutes int64
		for _, m := range o.Minutes {
			wantMinutes += m.Minutes
		}
		for _, by := range breakdownDimensions {
			t.Run(tc.name+" by "+string(by), func(t *testing.T) {
				rows, err := st.Breakdown(ctx, tc.f, by, "day")
				if err != nil {
					t.Fatal(err)
				}
				var runs, decided, succeeded, jobs, minutes int64
				for _, r := range rows {
					runs += r.Runs
					decided += r.Decided
					succeeded += r.Succeeded
					jobs += r.Jobs
					for _, m := range r.Minutes {
						minutes += m.Minutes
					}
					var spark int64
					for _, c := range r.Spark {
						spark += c.Runs
					}
					if spark != r.Runs {
						t.Errorf("%+v: sparkline counts %d runs, want %d", r, spark, r.Runs)
					}
				}
				if by == store.BreakdownByJob {
					if decided != o.JobsDecided || succeeded != o.JobsSucceeded {
						t.Errorf("jobs decided/succeeded = %d/%d, want %d/%d", decided, succeeded, o.JobsDecided, o.JobsSucceeded)
					}
				} else {
					if runs != o.Runs {
						t.Errorf("runs = %d, want %d", runs, o.Runs)
					}
					if decided != o.RunsDecided || succeeded != o.RunsSucceeded {
						t.Errorf("runs decided/succeeded = %d/%d, want %d/%d", decided, succeeded, o.RunsDecided, o.RunsSucceeded)
					}
				}
				if jobs != o.Jobs {
					t.Errorf("jobs = %d, want %d", jobs, o.Jobs)
				}
				if minutes != wantMinutes {
					t.Errorf("minutes = %d, want %d", minutes, wantMinutes)
				}
			})
		}
	}
}

func TestBreakdownGroupsDynamicWorkflowByPath(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	ctx := context.Background()
	f := store.Filter{Repository: storetest.DynamicRepo, Until: fx.Now.Add(time.Minute)}

	var names int
	err := st.Pool.QueryRow(ctx, `SELECT count(DISTINCT workflow_name) FROM runs WHERE repository = $1 AND path = $2`,
		storetest.DynamicRepo, storetest.DynamicPath).Scan(&names)
	if err != nil {
		t.Fatal(err)
	}
	if names != len(storetest.DynamicPullRequests) {
		t.Fatalf("fixtures name the dynamic workflow %d ways, want one per pull request (%d)", names, len(storetest.DynamicPullRequests))
	}

	for _, by := range []store.BreakdownDimension{store.BreakdownByWorkflow, store.BreakdownByJob} {
		rows, err := st.Breakdown(ctx, f, by, "day")
		if err != nil {
			t.Fatal(err)
		}
		var dynamic []store.BreakdownRow
		for _, r := range rows {
			if r.Path == storetest.DynamicPath {
				dynamic = append(dynamic, r)
			}
		}
		if len(dynamic) != 1 {
			t.Fatalf("by %s: %d rows for %s, want 1: %+v", by, len(dynamic), storetest.DynamicPath, rows)
		}
		got := dynamic[0]
		if got.Runs != int64(len(storetest.DynamicPullRequests)) || got.Jobs != int64(len(storetest.DynamicPullRequests)) {
			t.Errorf("by %s: %d runs and %d jobs, want one of each per pull request", by, got.Runs, got.Jobs)
		}
		if got.Workflow != "github-code-scanning/codeql" {
			t.Errorf("by %s: workflow label = %q, want the path without its dynamic/ prefix", by, got.Workflow)
		}
	}
}

func TestBreakdownRejectsUnknownDimension(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	if _, err := st.Breakdown(context.Background(), store.Filter{Until: time.Now()}, "path; DROP TABLE runs", "day"); err == nil {
		t.Fatal("want an error for an unknown dimension")
	}
}

func TestBreakdownEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	f := store.Filter{Since: fx.Now.AddDate(0, 0, 30), Until: fx.Now.AddDate(0, 0, 37)}
	for _, by := range breakdownDimensions {
		rows, err := st.Breakdown(context.Background(), f, by, "day")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("by %s: %d rows in an empty window", by, len(rows))
		}
	}
}
