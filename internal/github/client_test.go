package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/github/githubtest"
)

func TestValidSignature(t *testing.T) {
	body := []byte(`{"action":"completed"}`)
	good := github.Sign("s3cret", body)
	for _, tc := range []struct {
		name, secret, header string
		body                 []byte
		want                 bool
	}{
		{"matches", "s3cret", good, body, true},
		{"wrong secret", "other", good, body, false},
		{"tampered body", "s3cret", good, []byte(`{"action":"queued"}`), false},
		{"sha1 header", "s3cret", "sha1=" + good[len("sha256="):], body, false},
		{"not hex", "s3cret", "sha256=zz", body, false},
		{"empty secret accepts nothing", "", github.Sign("", body), body, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := github.ValidSignature(tc.secret, tc.body, tc.header); got != tc.want {
				t.Fatalf("ValidSignature = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrimaryRateLimitBlocksUntilReset(t *testing.T) {
	gh := githubtest.New(t)
	gh.Limited = true
	c := github.NewClient(gh.URL, github.StaticCredentials{C: gh.Credentials()})

	_, err := c.GetJob(context.Background(), githubtest.Installation, "o/r", 1)
	var rl *github.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want RateLimitError", err)
	}
	if d := time.Until(rl.Until); d < 50*time.Minute || d > time.Hour+time.Minute {
		t.Fatalf("Until is %v away, want about an hour", d)
	}

	before := len(gh.Calls)
	_, err = c.GetJob(context.Background(), githubtest.Installation, "o/r", 1)
	if !errors.As(err, &rl) {
		t.Fatalf("second call err = %v, want RateLimitError", err)
	}
	if len(gh.Calls) != before {
		t.Fatalf("second call reached GitHub while rate limited")
	}
}

func TestRetryAfterSetsBackoff(t *testing.T) {
	gh := githubtest.New(t)
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gh.Config.Handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer limited.Close()
	c := github.NewClient(limited.URL, github.StaticCredentials{C: gh.Credentials()})

	_, err := c.GetJob(context.Background(), githubtest.Installation, "o/r", 1)
	var rl *github.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want RateLimitError", err)
	}
	if d := time.Until(rl.Until); d < 110*time.Second || d > 121*time.Second {
		t.Fatalf("Until is %v away, want 120s", d)
	}
}

func TestNotConfigured(t *testing.T) {
	c := github.NewClient("http://127.0.0.1:1", nil)
	if _, err := c.RepoInstallation(context.Background(), "o/r"); !errors.Is(err, github.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}
