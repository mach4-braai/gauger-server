package ui_test

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

func jobPageURL(id int64) string { return "/stats/jobs/" + strconv.FormatInt(id, 10) }

// jobStepCells reads the text of each data-col cell in step n's table row.
func jobStepCells(t *testing.T, page string, n int) map[string]string {
	t.Helper()
	row := regexp.MustCompile(`(?s)<tr id="step-` + strconv.Itoa(n) + `"[^>]*>(.*?)</tr>`).FindStringSubmatch(page)
	if row == nil {
		t.Fatalf("no row for step %d in the steps table", n)
	}
	cells := map[string]string{}
	for _, m := range regexp.MustCompile(`data-col="([a-z-]+)">([^<]*)<`).FindAllStringSubmatch(row[1], -1) {
		cells[m[1]] = html.UnescapeString(m[2])
	}
	return cells
}

func jobText(page, id string) string {
	m := regexp.MustCompile(`(?s)id="` + id + `"[^>]*>(?:<div class="kv-key">[^<]*</div><div class="kv-value">)?([^<]*)<`).FindStringSubmatch(page)
	if m == nil {
		return "<missing " + id + ">"
	}
	return html.UnescapeString(m[1])
}

func jobOptional(v *float64, f func(float64) string) string {
	if v == nil {
		return "–"
	}
	return f(*v)
}

func TestJobPageStepUsageMatchesStore(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)
	d, err := st.Job(context.Background(), fx.SampledJob)
	if err != nil || d == nil {
		t.Fatalf("Job: %v, %v", d, err)
	}
	page := get(t, h, jobPageURL(fx.SampledJob)).Body.String()

	if len(d.Steps) != 5 {
		t.Fatalf("seeded job has %d steps, want 5", len(d.Steps))
	}
	var total, withUsage int64
	for _, s := range d.Steps {
		cells := jobStepCells(t, page, s.Number)
		want := map[string]string{
			"result":      s.Conclusion,
			"started":     s.StartedAt.UTC().Format("2006-01-02 15:04:05"),
			"duration":    chart.Seconds(s.CompletedAt.Sub(*s.StartedAt).Seconds()),
			"peak-memory": jobOptional(s.PeakMemory, chart.Bytes),
			"of-memtotal": "–",
			"peak-cpu":    "–",
			"saturated":   jobOptional(s.Saturated, func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }),
			"samples":     strconv.FormatInt(s.Samples, 10),
		}
		if s.PeakMemory != nil {
			want["of-memtotal"] = fmt.Sprintf("%.0f%%", *s.PeakMemory / *d.MemTotal * 100)
			want["peak-cpu"] = fmt.Sprintf("%.1f of %.0f", *s.PeakCPU**d.CPUCount, *d.CPUCount)
			withUsage++
		}
		for col, w := range want {
			if cells[col] != w {
				t.Errorf("step %d %s = %q, want %q", s.Number, col, cells[col], w)
			}
		}
		total += s.Samples
	}
	if withUsage == 0 {
		t.Error("no step has samples, so the usage columns went unchecked")
	}
	if total != d.Samples {
		t.Errorf("step sample counts add up to %d, want the job's %d", total, d.Samples)
	}
}

func TestJobPageHeader(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)
	d, err := st.Job(context.Background(), fx.SampledJob)
	if err != nil || d == nil {
		t.Fatalf("Job: %v, %v", d, err)
	}
	page := get(t, h, jobPageURL(fx.SampledJob)).Body.String()

	for id, want := range map[string]string{
		"fact-branch":   d.Branch,
		"fact-sha":      d.HeadSHA[:7],
		"fact-event":    d.Event,
		"fact-runner":   d.RunnerName,
		"fact-labels":   strings.Join(d.Labels, ", "),
		"fact-result":   d.Conclusion,
		"fact-attempt":  strconv.Itoa(d.RunAttempt),
		"fact-queue":    chart.Seconds(d.StartedAt.Sub(*d.CreatedAt).Seconds()),
		"fact-duration": chart.Seconds(d.CompletedAt.Sub(*d.StartedAt).Seconds()),
		"fact-memtotal": chart.Bytes(*d.MemTotal),
		"fact-nproc":    "4",
	} {
		if got := jobText(page, id); got != want {
			t.Errorf("%s = %q, want %q", id, got, want)
		}
	}
	for _, want := range []string{
		d.Repository,
		`href="` + github.RunURL("https://github.com", d.Repository, d.RunID, d.RunAttempt) + `"`,
		`href="` + github.BranchURL("https://github.com", d.Repository, d.Branch) + `"`,
		`href="` + github.CommitURL("https://github.com", d.Repository, d.HeadSHA) + `"`,
		`href="` + github.WorkflowURL("https://github.com", d.Repository, ".github/workflows/ci.yml") + `"`,
		`href="` + d.HTMLURL + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestJobPageGaugerStatusLines(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		id   int64
		want func(samples int64) string
	}{
		{"reported live", fx.SampledJob, func(n int64) string { return fmt.Sprintf("gauger reported %d samples and finished.", n) }},
		{"reported with the artifact", fx.ArtifactJob, func(n int64) string {
			return fmt.Sprintf("gauger reported %d samples, some from the fallback artifact.", n)
		}},
		{"artifact only", fx.ArtifactOnlyJob, func(n int64) string {
			return fmt.Sprintf("gauger never reached the server. Its fallback artifact held %d samples.", n)
		}},
		{"no samples", fx.UnsampledJob, func(int64) string { return "No gauger data for this job." }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := st.Job(ctx, tc.id)
			if err != nil || d == nil {
				t.Fatalf("Job: %v, %v", d, err)
			}
			page := get(t, h, jobPageURL(tc.id)).Body.String()
			if got, want := jobText(page, "gauger-status"), tc.want(d.Samples); got != want {
				t.Errorf("status line = %q, want %q", got, want)
			}
			if got := strings.Count(page, `class="timeline-span"`); got != 1+len(d.Steps) {
				t.Errorf("timeline has %d spans, want the queue wait and %d steps", got, len(d.Steps))
			}
			wantCharts := 0
			if d.Samples > 0 {
				wantCharts = 4
			}
			if got := strings.Count(page, `class="chart"`); got != wantCharts {
				t.Errorf("job with %d samples draws %d resource charts, want %d", d.Samples, got, wantCharts)
			}
		})
	}
}

func TestJobPageStepSpansDoNotOverlap(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)
	d, err := st.Job(context.Background(), fx.SampledJob)
	if err != nil || d == nil {
		t.Fatalf("Job: %v, %v", d, err)
	}
	for i := 1; i < len(d.Steps); i++ {
		if !d.Steps[i-1].CompletedAt.Equal(*d.Steps[i].StartedAt) {
			t.Fatalf("steps %d and %d do not end and start in the same second: the fixture no longer covers #8", i, i+1)
		}
	}

	page := get(t, h, jobPageURL(fx.SampledJob)).Body.String()
	type span struct{ left, width float64 }
	var spans []span
	for _, m := range regexp.MustCompile(`class="timeline-span"[^>]*style="left: ([\d.]+)%; width: ([\d.]+)%`).FindAllStringSubmatch(page, -1) {
		l, _ := strconv.ParseFloat(m[1], 64)
		w, _ := strconv.ParseFloat(m[2], 64)
		spans = append(spans, span{l, w})
	}
	if len(spans) != 1+len(d.Steps) {
		t.Fatalf("%d spans, want the queue wait and %d steps", len(spans), len(d.Steps))
	}
	const rounding = 0.002
	for i := 1; i < len(spans); i++ {
		prev, s := spans[i-1], spans[i]
		if s.left < prev.left+prev.width-rounding {
			t.Errorf("span %d starts at %.3f%%, inside span %d, which ends at %.3f%%", i, s.left, i-1, prev.left+prev.width)
		}
		if s.left > prev.left+prev.width+rounding {
			t.Errorf("span %d starts at %.3f%%, after a gap past span %d, which ends at %.3f%%", i, s.left, i-1, prev.left+prev.width)
		}
	}
}

func TestJobPageDrawer(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)
	d, err := st.Job(context.Background(), fx.SampledJob)
	if err != nil || d == nil {
		t.Fatalf("Job: %v, %v", d, err)
	}
	step := d.Steps[3]
	url := jobPageURL(fx.SampledJob) + "?step=" + strconv.Itoa(step.Number)

	if page := get(t, h, jobPageURL(fx.SampledJob)).Body.String(); strings.Contains(page, `id="step-drawer"`) {
		t.Error("the drawer is open without a step")
	}
	if page := get(t, h, jobPageURL(fx.SampledJob)+"?step=99").Body.String(); strings.Contains(page, `id="step-drawer"`) {
		t.Error("the drawer is open for a step the job does not have")
	}

	rec := get(t, h, url+"&datastar=%7B%7D", "Datastar-Request", "true")
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	patch := rec.Body.String()
	for _, want := range []string{
		`data: elements <div id="page"`,
		`id="step-drawer"`,
		`href="` + github.StepURL(d.HTMLURL, step.Number) + `"`,
		html.EscapeString(step.Name),
		`id="drawer-peak-memory"`,
		`history.replaceState(null, '', "` + url + `")`,
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, patch)
		}
	}
	page := get(t, h, url).Body.String()
	for id, want := range map[string]string{
		"drawer-peak-memory": jobOptional(step.PeakMemory, chart.Bytes),
		"drawer-samples":     strconv.FormatInt(step.Samples, 10),
		"drawer-duration":    chart.Seconds(step.CompletedAt.Sub(*step.StartedAt).Seconds()),
	} {
		if got := jobText(page, id); got != want {
			t.Errorf("%s = %q, want %q", id, got, want)
		}
	}
	if !strings.Contains(page, `<tr id="step-`+strconv.Itoa(step.Number)+`" data-selected="true">`) {
		t.Error("the open step's row is not marked selected")
	}
}

func TestJobPageResourceCharts(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)
	page := get(t, h, jobPageURL(fx.SampledJob)).Body.String()

	for _, id := range []string{"chart-cpu", "chart-memory", "chart-disk", "chart-network"} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("no %s chart", id)
		}
	}
	for _, want := range []string{
		`title="user"`, `title="iowait"`, `title="interrupt"`, `title="used"`, `title="cached"`, `title="buffers"`,
		`title="nvme0n1 read"`, `title="nvme0n1 write"`, `title="eth0 in"`, `title="eth0 out"`, "MemTotal 16 GiB",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	for _, absent := range []string{`title="idle"`, `title="free"`, `title="lo in"`, `title="lo out"`} {
		if strings.Contains(page, absent) {
			t.Errorf("page has %q, which no chart should draw", absent)
		}
	}
	marks := strings.Count(page, `class="trace-mark"`)
	if marks != 4*5 {
		t.Errorf("%d step marks across 4 charts, want %d", marks, 4*5)
	}

	hidden := get(t, h, jobPageURL(fx.SampledJob)+"?hide=cpu%3Auser").Body.String()
	if !strings.Contains(hidden, `name="hide" value="cpu:user"`) {
		t.Error("a hidden series is not carried by the filter form")
	}
	if strings.Count(hidden, "<path") >= strings.Count(page, "<path") {
		t.Error("hiding the user series left the chart's paths unchanged")
	}
}

func TestJobPageNotFound(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)

	for _, path := range []string{"/stats/jobs/1", "/stats/jobs/nope"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}
}

func TestJobPageWithoutSamplesStillDrawsTheTimeline(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now), jobPageURL(fx.UnsampledJob)).Body.String()

	if strings.Contains(page, `class="trace"`) {
		t.Error("a job without samples draws resource charts")
	}
	if !strings.Contains(page, `class="timeline-span"`) || !strings.Contains(page, "Queued") {
		t.Error("the timeline is missing for a job without samples")
	}
	if got := jobStepCells(t, page, 4); got["peak-memory"] != "–" || got["samples"] != "0" {
		t.Errorf("step 4 of an unsampled job = %v, want no usage", got)
	}
}

func TestJobPageRunningJobHasOpenStep(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	var id int64
	err := st.Pool.QueryRow(context.Background(), `SELECT id FROM jobs WHERE status = 'in_progress' ORDER BY id LIMIT 1`).Scan(&id)
	if err != nil {
		t.Fatalf("no running job in the fixtures: %v", err)
	}
	page := get(t, dashboard(st, fx.Now), jobPageURL(id)).Body.String()
	if got := jobStepCells(t, page, 4)["result"]; got != "in_progress" {
		t.Errorf("step 4 result = %q, want in_progress", got)
	}
	if strings.Count(page, `class="timeline-span"`) < 4 {
		t.Error("the open step has no bar on the timeline")
	}
}
