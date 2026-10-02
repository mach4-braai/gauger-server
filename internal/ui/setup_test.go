package ui_test

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/github/githubtest"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui"
)

func TestManifestFlowCreatesApp(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	gh := githubtest.New(t)
	h := (&ui.UI{Store: st, GitHub: github.NewClient(gh.URL, st), Creds: st, DNSName: "gauger.example.ts.net", GitHubURL: "https://github.com"}).Handler()

	form := url.Values{"name": {"gauger-test"}, "org": {"acme"}}
	req := httptest.NewRequest(http.MethodPost, "/setup/manifest", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest: status %d: %s", rec.Code, rec.Body)
	}
	cookie := rec.Result().Cookies()[0]
	page := rec.Body.String()

	action := regexp.MustCompile(`action="([^"]+)"`).FindStringSubmatch(page)[1]
	wantAction := "https://github.com/organizations/acme/settings/apps/new?state=" + cookie.Value
	if html.UnescapeString(action) != wantAction {
		t.Fatalf("form action = %s, want %s", html.UnescapeString(action), wantAction)
	}
	raw := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindStringSubmatch(page)[1]
	var m github.Manifest
	if err := json.Unmarshal([]byte(html.UnescapeString(raw)), &m); err != nil {
		t.Fatal(err)
	}
	if m.HookAttributes.URL != "https://gauger.example.ts.net:8443/webhooks/github" ||
		m.DefaultPermissions["actions"] != "read" || m.DefaultPermissions["metadata"] != "read" || len(m.DefaultPermissions) != 2 ||
		strings.Join(m.DefaultEvents, ",") != "workflow_run,workflow_job" ||
		m.RedirectURL != "https://gauger.example.ts.net/setup/callback" {
		t.Fatalf("manifest = %+v", m)
	}

	bad := httptest.NewRequest(http.MethodGet, "/setup/callback?code=abc&state=forged", nil)
	bad.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state: status %d, want 400", rec.Code)
	}

	cb := httptest.NewRequest(http.MethodGet, "/setup/callback?code=abc&state="+cookie.Value, nil)
	cb.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, cb)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: status %d: %s", rec.Code, rec.Body)
	}
	creds, err := st.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AppID != githubtest.AppID || creds.WebhookSecret != githubtest.WebhookSecret || !creds.PrivateKey.Equal(gh.Key) {
		t.Fatalf("stored App = %d/%q", creds.AppID, creds.WebhookSecret)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/setup/manifest", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second manifest: status %d, want 409", rec.Code)
	}
}

func TestBackfillQueuesTaskPerInstalledRepository(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, 90*24*time.Hour)
	gh := githubtest.New(t)
	if err := st.SaveApp(ctx, &github.AppConversion{ID: githubtest.AppID, Slug: "gauger-test", HTMLURL: "https://github.com/apps/gauger-test", WebhookSecret: githubtest.WebhookSecret, PEM: gh.PEM()}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetInstallation(ctx, "acme/with-app", githubtest.Installation); err != nil {
		t.Fatal(err)
	}
	h := (&ui.UI{Store: st, GitHub: github.NewClient(gh.URL, st), Creds: st, DNSName: "gauger.example.ts.net", GitHubURL: "https://github.com"}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/setup/backfill"`) {
		t.Fatalf("setup page missing backfill form: status %d: %s", rec.Code, rec.Body)
	}

	form := url.Values{"days": {"14"}}
	req := httptest.NewRequest(http.MethodPost, "/setup/backfill", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("backfill: status %d: %s", rec.Code, rec.Body)
	}

	var key, repo string
	if err := st.Pool.QueryRow(ctx, `SELECT key, repository FROM tasks WHERE kind = 'backfill'`).Scan(&key, &repo); err != nil {
		t.Fatalf("no backfill task queued: %v", err)
	}
	if repo != "acme/with-app" {
		t.Fatalf("backfill queued for repository %q, want acme/with-app", repo)
	}
	wantSince := time.Now().UTC().AddDate(0, 0, -14).Format("2006-01-02")
	if key != wantSince+":acme/with-app" {
		t.Fatalf("backfill key = %q, want %q", key, wantSince+":acme/with-app")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/setup/backfill", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req2)
	var count int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind = 'backfill'`).Scan(&count)
	if count != 1 {
		t.Fatalf("backfill tasks after resubmit = %d, want 1", count)
	}
}

func TestBackfillRequiresConfiguredApp(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	gh := githubtest.New(t)
	h := (&ui.UI{Store: st, GitHub: github.NewClient(gh.URL, st), Creds: st, DNSName: "gauger.example.ts.net", GitHubURL: "https://github.com"}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/setup/backfill", strings.NewReader(url.Values{"days": {"14"}}.Encode())))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
}
