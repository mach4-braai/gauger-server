package ui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/github/githubtest"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui"
)

func TestNavbarShowsBuildVersion(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	gh := githubtest.New(t)
	for _, tc := range []struct {
		version string
		want    []string
		absent  []string
	}{
		{
			version: "abc123def456",
			want: []string{
				`<span class="version" title="build abc123def456"><a href="https://github.com/mach4-braai/gauger-server/commit/abc123def456">abc123def456</a></span>`,
			},
		},
		{
			version: "abc123def456-dirty",
			want: []string{
				`<a href="https://github.com/mach4-braai/gauger-server/commit/abc123def456">abc123def456-dirty</a>`,
			},
		},
		{
			version: "dev",
			want:    []string{`<span class="version" title="build dev">dev</span>`},
			absent:  []string{"/commit/"},
		},
		{version: "", absent: []string{`class="version"`}},
	} {
		h := (&ui.UI{Store: st, GitHub: github.NewClient(gh.URL, st), Creds: st, GitHubURL: "https://github.com", Version: tc.version}).Handler()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("version %q: status %d: %s", tc.version, rec.Code, rec.Body)
		}
		page := rec.Body.String()
		header := page[strings.Index(page, "<header>"):strings.Index(page, "</header>")]
		for _, w := range tc.want {
			if !strings.Contains(header, w) {
				t.Errorf("version %q: header missing %s\n%s", tc.version, w, header)
			}
		}
		for _, a := range tc.absent {
			if strings.Contains(header, a) {
				t.Errorf("version %q: header should not contain %s\n%s", tc.version, a, header)
			}
		}
	}
}
