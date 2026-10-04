package ui_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

var (
	spendTags     = regexp.MustCompile(`<[^>]*>`)
	spendMonthRow = regexp.MustCompile(`<tr data-month="([^"]*)" data-repo="([^"]*)" data-labels="([^"]*)">.*?data-col="jobs">([^<]*)</td>.*?data-col="minutes">([^<]*)</td>.*?data-col="rate">(.*?)</td><td[^>]*data-col="cost">(.*?)</td></tr>`)
	spendNameRow  = regexp.MustCompile(`<tr data-name="([^"]*)">.*?data-col="jobs">([^<]*)</td>.*?data-col="minutes">([^<]*)</td>.*?data-col="spend">(.*?)</td>`)
)

func spendText(html string) string { return strings.TrimSpace(spendTags.ReplaceAllString(html, "")) }

func spendDollars(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(strings.NewReplacer("$", "", ",", "").Replace(s), 64)
	if err != nil {
		t.Fatalf("%q is not a dollar amount", s)
	}
	return v
}

func spendInt(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if err != nil {
		t.Fatalf("%q is not an integer", s)
	}
	return v
}

// spendSection is the HTML of the table with the given id.
func spendSection(t *testing.T, page, id string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, `<table id="`+id+`"`)
	if !ok {
		t.Fatalf("page has no table %s", id)
	}
	body, _, _ := strings.Cut(rest, "</table>")
	return body
}

// spendJobMinutes sums each completed job's minutes, rounded up, where cond holds.
func spendJobMinutes(t *testing.T, st *store.Store, cond string, args ...any) int64 {
	t.Helper()
	var n int64
	err := st.Pool.QueryRow(context.Background(), `
		SELECT coalesce(sum(ceil(extract(epoch FROM j.completed_at - j.started_at) / 60)), 0)::bigint
		FROM jobs j
		WHERE j.status = 'completed' AND j.started_at IS NOT NULL AND j.completed_at > j.started_at AND `+cond, args...).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSpendMatchesSpendGroups(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))
	ctx := context.Background()

	for _, repo := range []string{"", "acme/api", "oss/gauger"} {
		t.Run("repo="+repo, func(t *testing.T) {
			groups, err := st.SpendGroups(ctx, store.Filter{Repository: repo})
			if err != nil {
				t.Fatal(err)
			}
			if len(groups) == 0 {
				t.Fatal("fixtures have no spend; the case checks nothing")
			}
			page := get(t, h, "/stats/spend?range=all&repo="+strings.ReplaceAll(repo, "/", "%2F")).Body.String()

			type row struct{ jobs, minutes, rate, cost string }
			got := map[string]row{}
			for _, m := range spendMonthRow.FindAllStringSubmatch(spendSection(t, page, "spend-months")+"</table>", -1) {
				got[m[1]+"|"+m[2]+"|"+m[3]] = row{m[4], m[5], spendText(m[6]), spendText(m[7])}
			}
			if len(got) != len(groups) {
				t.Errorf("month table has %d rows, SpendGroups has %d", len(got), len(groups))
			}

			var cost float64
			var billable, free, unknown int64
			for _, g := range groups {
				p := spend.Rates(spend.DefaultRates).Price(g.Labels, g.Private, g.Minutes)
				want := row{strconv.FormatInt(g.Jobs, 10), strconv.FormatInt(g.Minutes, 10), "unknown", "unknown"}
				if p.Known {
					want.rate = "$" + strconv.FormatFloat(p.Rate, 'f', -1, 64) + "/min"
					want.cost = chart.USD(p.Cost)
					if p.Free {
						want.cost = "free"
					}
				}
				key := g.Month.UTC().Format("2006-01") + "|" + g.Repository + "|" + strings.Join(g.Labels, ", ")
				if got[key] != want {
					t.Errorf("month row %s = %+v, want %+v", key, got[key], want)
				}
				cost += p.Cost
				switch {
				case !p.Known:
					unknown += g.Minutes
				case p.Free:
					free += g.Minutes
				default:
					billable += g.Minutes
				}
			}

			cs := cards(page)
			if v := spendDollars(t, cs["spend"][0]); math.Abs(v-cost) > 0.011 {
				t.Errorf("spend card = %s, want %s", cs["spend"][0], chart.USD(cost))
			}
			for id, want := range map[string]int64{"billable": billable, "free": free, "unknown": unknown} {
				if got := spendInt(t, cs[id][0]); got != want {
					t.Errorf("%s minutes card = %d, want %d", id, got, want)
				}
			}
		})
	}
}

func TestSpendTablesAddUp(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.Add(time.Minute)), "/stats/spend?range=all").Body.String()
	total := spendJobMinutes(t, st, "true")

	for _, id := range []string{"spend-repository", "spend-workflow", "spend-job"} {
		var sum int64
		for _, m := range spendNameRow.FindAllStringSubmatch(spendSection(t, page, id)+"</table>", -1) {
			sum += spendInt(t, m[3])
		}
		if sum != total {
			t.Errorf("%s minutes add up to %d, want %d", id, sum, total)
		}
	}
}

func TestSpendPublicRepositoryShowsMinutesAndNoCost(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	want := spendJobMinutes(t, st, `j.repository = 'oss/docs' AND 'ubuntu-latest' = ANY(j.labels)`)
	if want == 0 {
		t.Fatal("oss/docs has no ubuntu-latest minutes; the case checks nothing")
	}
	page := get(t, h, "/stats/spend?range=all&repo=oss%2Fdocs").Body.String()

	var found bool
	for _, m := range spendNameRow.FindAllStringSubmatch(spendSection(t, page, "spend-repository")+"</table>", -1) {
		if m[1] != "oss/docs" {
			continue
		}
		found = true
		if got := spendInt(t, m[3]); got != spendJobMinutes(t, st, `j.repository = 'oss/docs'`) {
			t.Errorf("oss/docs minutes = %d", got)
		}
		if got := spendText(m[4]); got != "$0" {
			t.Errorf("oss/docs spend = %q, want $0", got)
		}
	}
	if !found {
		t.Fatalf("no oss/docs row:\n%s", page)
	}
	cs := cards(page)
	if cs["spend"][0] != "$0" || cs["billable"][0] != "0" || cs["unknown"][0] != "0" {
		t.Errorf("cards = spend %q billable %q unknown %q, want $0, 0 and 0", cs["spend"][0], cs["billable"][0], cs["unknown"][0])
	}
	if got := spendInt(t, cs["free"][0]); got != spendJobMinutes(t, st, `j.repository = 'oss/docs'`) {
		t.Errorf("free minutes = %d", got)
	}
	for _, m := range spendMonthRow.FindAllStringSubmatch(spendSection(t, page, "spend-months")+"</table>", -1) {
		if strings.Contains(m[3], "ubuntu-latest") && spendText(m[7]) != "free" {
			t.Errorf("public ubuntu-latest month row costs %q, want free", spendText(m[7]))
		}
	}
}

func TestSpendUnknownLabelIsNotZero(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)

	want := spendJobMinutes(t, st, `j.id = $1`, fx.UnratedJob)
	if want == 0 {
		t.Fatal("the unrated job has no minutes")
	}
	page := get(t, h, "/stats/spend?range=7d").Body.String()

	cs := cards(page)
	if got := spendInt(t, cs["unknown"][0]); got != want {
		t.Errorf("unknown-rate card = %d, want %d", got, want)
	}
	if !strings.Contains(cs["spend"][1], "no known rate") {
		t.Errorf("spend hint = %q, want it to mention minutes with no known rate", cs["spend"][1])
	}

	var found bool
	for _, m := range spendNameRow.FindAllStringSubmatch(spendSection(t, page, "spend-workflow")+"</table>", -1) {
		if m[1] == "Load test" {
			if got := spendText(m[4]); got != "unknown" {
				t.Errorf("Load test workflow spend = %q, want unknown", got)
			}
			if spendInt(t, m[3]) != want {
				t.Errorf("Load test workflow minutes = %s, want %d", m[3], want)
			}
			found = true
		}
	}
	if !found {
		t.Error("no Load test row in the workflow table")
	}

	for _, m := range spendMonthRow.FindAllStringSubmatch(spendSection(t, page, "spend-months")+"</table>", -1) {
		if m[3] != storetest.UnratedLabel {
			continue
		}
		if spendText(m[6]) != "unknown" || spendText(m[7]) != "unknown" {
			t.Errorf("unrated month row rate %q cost %q, want unknown for both", spendText(m[6]), spendText(m[7]))
		}
		return
	}
	t.Error("no month row for the unrated label")
}

func TestSpendChartMetricToggle(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	legend := regexp.MustCompile(`<span class="truncate">([^<]*)</span>\s*<span class="legend-value">([^<]*)</span>`)
	minutes := get(t, h, "/stats/spend?range=all&metric=minutes").Body.String()
	if !strings.Contains(minutes, `value="minutes" form="filters" checked`) || !strings.Contains(minutes, "Minutes by runner label") {
		t.Error("metric=minutes does not select the minutes toggle")
	}
	var sum int64
	var unknown bool
	for _, m := range legend.FindAllStringSubmatch(minutes, -1) {
		sum += spendInt(t, m[2])
		unknown = unknown || m[1] == "unknown rate"
	}
	if want := spendJobMinutes(t, st, "true"); sum != want {
		t.Errorf("minutes legend adds up to %d, want %d", sum, want)
	}
	if !unknown {
		t.Error("the minutes chart has no series for the label without a rate")
	}

	spendPage := get(t, h, "/stats/spend?range=all").Body.String()
	if !strings.Contains(spendPage, `value="spend" form="filters" checked`) {
		t.Error("the spend toggle is not selected by default")
	}
	for _, m := range legend.FindAllStringSubmatch(spendPage, -1) {
		if m[1] == "unknown rate" {
			t.Error("the spend chart plots the unrated label as spend")
		}
	}

	hidden := get(t, h, "/stats/spend?range=all&metric=minutes&hide=spend%3Aubuntu-latest").Body.String()
	if !strings.Contains(hidden, `name="hide" value="spend:ubuntu-latest"`) {
		t.Error("a hidden series is not carried by the filter form")
	}

	rec := get(t, h, "/stats/spend?range=all&metric=minutes", "Datastar-Request", "true")
	if body := rec.Body.String(); !strings.Contains(body, `id="spend-repository"`) || strings.Contains(body, "<html") {
		t.Errorf("a Datastar request should patch the page only:\n%s", body)
	}
}

func TestSpendEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.AddDate(0, 0, 30)), "/stats/spend?range=7d").Body.String()

	cs := cards(page)
	if cs["spend"][0] != "$0" || cs["billable"][0] != "0" || cs["free"][0] != "0" || cs["unknown"][0] != "0" {
		t.Errorf("cards = %v, want all zero", cs)
	}
	if !strings.Contains(page, "No completed jobs in this range") {
		t.Error("the empty chart label is missing")
	}
	if strings.Contains(page, "<tr data-") {
		t.Error("an empty window still lists table rows")
	}
	if n := strings.Count(page, "Nothing in this range ran to completion."); n != 4 {
		t.Errorf("empty tables = %d, want 4", n)
	}
}

func TestSpendRedirectCarriesTheQuery(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	h := dashboard(st, time.Now())

	for from, want := range map[string]string{
		"/spend?repo=acme%2Fapi&range=30d": "/stats/spend?repo=acme%2Fapi&range=30d",
		"/spend":                           "/stats/spend",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, from, nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("GET %s: status = %d, want 302", from, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("GET %s: Location = %q, want %q", from, got, want)
		}
	}
}
