package ui_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mach4-braai/gauger-server/internal/ui"
)

func TestOldReportURLsRedirectToTheDashboard(t *testing.T) {
	h := (&ui.UI{}).Handler()

	for _, tc := range []struct{ from, to string }{
		{"/", "/stats/runs"},
		{"/?repo=acme%2Fapi&days=30", "/stats/runs?repo=acme%2Fapi&days=30"},
		{"/jobs/42", "/stats/jobs/42"},
		{"/jobs/42?range=30d&step=2", "/stats/jobs/42?range=30d&step=2"},
		{"/steps", "/stats/steps"},
		{"/steps?repo=acme%2Fapi&days=7", "/stats/steps?repo=acme%2Fapi&days=7"},
		{"/sizing", "/stats/sizing"},
		{"/sizing?days=14&repo=acme%2Fapi", "/stats/sizing?days=14&repo=acme%2Fapi"},
		{"/spend", "/stats/spend"},
		{"/spend?repo=acme%2Fapi&range=30d", "/stats/spend?repo=acme%2Fapi&range=30d"},
		// /daily and /regressions share the Trends page. Their days become
		// the range that covers them.
		{"/daily", "/stats/trends?range=30d"},
		{"/daily?repo=acme/app&days=1&bucket=week&workflow=CI&job=a", "/stats/trends?bucket=week&job=a&range=24h&repo=acme%2Fapp&workflow=CI"},
		{"/regressions?days=45&ratio=2&min=10&repo=acme/api", "/stats/trends?min=10&range=90d&ratio=2&repo=acme%2Fapi"},
		{"/regressions?days=7", "/stats/trends?range=7d"},
		{"/regressions?days=3650", "/stats/trends?range=all"},
		{"/regressions?days=zero&ratio=", "/stats/trends?range=30d"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.from, nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != tc.to {
			t.Errorf("GET %s = %d to %q, want 302 to %q", tc.from, rec.Code, rec.Header().Get("Location"), tc.to)
		}
	}
}
