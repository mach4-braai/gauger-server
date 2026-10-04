package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/reconcile"
	"github.com/mach4-braai/gauger-server/internal/store"
)

const stateCookie = "gauger_setup_state"

// DefaultBackfillDays is how far back a backfill reaches when the form
// leaves Days empty.
const DefaultBackfillDays = 90

var orgName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

type setupPage struct {
	App         *github.Credentials
	DefaultName string
	DefaultDays int
	Error       string
}

func (u *UI) setup(w http.ResponseWriter, r *http.Request) {
	page := setupPage{DefaultName: "gauger-" + strings.SplitN(u.DNSName, ".", 2)[0], DefaultDays: DefaultBackfillDays}
	creds, err := u.Creds.Credentials(r.Context())
	switch {
	case err == nil:
		page.App = creds
	case !errors.Is(err, github.ErrNotConfigured):
		page.Error = err.Error()
	}
	u.render(w, "setup", page)
}

func (u *UI) setupManifest(w http.ResponseWriter, r *http.Request) {
	if _, err := u.Creds.Credentials(r.Context()); err == nil {
		http.Error(w, "a GitHub App is already configured", http.StatusConflict)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	org := strings.TrimSpace(r.FormValue("org"))
	if name == "" || len(name) > 34 {
		http.Error(w, "the App name must be 1 to 34 characters", http.StatusBadRequest)
		return
	}
	if org != "" && !orgName.MatchString(org) {
		http.Error(w, "not a valid organization name", http.StatusBadRequest)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/setup", MaxAge: 3600,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	manifest, err := json.Marshal(github.NewManifest(name, u.DNSName))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "setup_redirect", map[string]any{
		"Action":   github.ManifestFormURL(u.GitHubURL, org, state),
		"Manifest": string(manifest),
	})
}

func (u *UI) setupCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "setup state does not match; start again from /setup", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	conv, err := u.GitHub.ConvertManifest(r.Context(), code)
	if err != nil {
		slog.Error("convert App manifest", "err", err)
		http.Error(w, "GitHub did not accept the manifest code: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := u.Store.SaveApp(r.Context(), conv); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrAppExists) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	slog.Info("GitHub App created", "app_id", conv.ID, "slug", conv.Slug)
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/setup", MaxAge: -1})
	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}

// setupBackfill queues a backfill task for every repository with an
// installation, reaching back Days days (DefaultBackfillDays if empty).
func (u *UI) setupBackfill(w http.ResponseWriter, r *http.Request) {
	if _, err := u.Creds.Credentials(r.Context()); err != nil {
		http.Error(w, "configure a GitHub App first", http.StatusConflict)
		return
	}
	days := DefaultBackfillDays
	if v := strings.TrimSpace(r.FormValue("days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "days must be a positive integer", http.StatusBadRequest)
			return
		}
		days = n
	}
	repos, err := u.Store.InstalledRepositories(r.Context())
	if err != nil {
		u.fail(w, err)
		return
	}
	since := time.Now().UTC().AddDate(0, 0, -days)
	now := time.Now()
	expires := now.Add(reconcile.TaskLifetime)
	for _, repo := range repos {
		err := store.EnqueueTask(r.Context(), u.Store, store.KindBackfill, store.BackfillKey(repo, since), repo, now, expires)
		if err != nil {
			u.fail(w, err)
			return
		}
	}
	if u.Wake != nil {
		u.Wake()
	}
	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}
