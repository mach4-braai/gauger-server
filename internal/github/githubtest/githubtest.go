// Package githubtest runs a fake GitHub REST API for tests.
package githubtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
)

const (
	AppID         = 42
	Installation  = 7
	WebhookSecret = "webhook-secret"
)

// Server is a fake GitHub API. Change its maps under Mu while it runs.
type Server struct {
	*httptest.Server
	Key *rsa.PrivateKey

	Mu   sync.Mutex
	Runs map[string]*github.Run // key: runID:attempt
	Jobs map[int64]*github.Job  // key: job ID
	// Limited makes every API call answer 403 with a rate-limit reset.
	Limited bool
	Calls   []string
}

func New(t testing.TB) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Key:  key,
		Runs: map[string]*github.Run{},
		Jobs: map[int64]*github.Job{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Credentials returns App credentials the fake accepts.
func (s *Server) Credentials() *github.Credentials {
	return &github.Credentials{AppID: AppID, Slug: "gauger-test", HTMLURL: "https://github.com/apps/gauger-test", PrivateKey: s.Key, WebhookSecret: WebhookSecret}
}

// PEM returns the App private key in the format GitHub hands out.
func (s *Server) PEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.Key)}))
}

// CallCount returns how many requests hit a path starting with p.
func (s *Server) CallCount(p string) int {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	n := 0
	for _, c := range s.Calls {
		if strings.HasPrefix(c, p) {
			n++
		}
	}
	return n
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Calls = append(s.Calls, r.URL.Path)

	if strings.HasPrefix(r.URL.Path, "/app-manifests/") {
		writeJSON(w, http.StatusCreated, github.AppConversion{
			ID: AppID, Slug: "gauger-test", HTMLURL: "https://github.com/apps/gauger-test",
			ClientID: "Iv1.test", ClientSecret: "client-secret", WebhookSecret: WebhookSecret, PEM: s.PEM(),
		})
		return
	}
	if s.Limited {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded"})
		return
	}
	auth := r.Header.Get("Authorization")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	switch {
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "app" && parts[3] == "access_tokens":
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, nil)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": "inst-token", "expires_at": time.Now().Add(time.Hour)})
		return
	case len(parts) == 4 && parts[0] == "repos" && parts[3] == "installation":
		writeJSON(w, http.StatusOK, github.Installation{ID: Installation})
		return
	}
	if auth != "token inst-token" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "bad credentials"})
		return
	}
	if len(parts) < 5 || parts[0] != "repos" || parts[3] != "actions" {
		writeJSON(w, http.StatusNotFound, nil)
		return
	}
	rest := parts[4:]
	switch {
	case len(rest) == 2 && rest[0] == "jobs":
		for id, j := range s.Jobs {
			if fmt.Sprint(id) == rest[1] {
				writeJSON(w, http.StatusOK, j)
				return
			}
		}
	case len(rest) == 4 && rest[0] == "runs" && rest[2] == "attempts":
		if run, ok := s.Runs[rest[1]+":"+rest[3]]; ok {
			writeJSON(w, http.StatusOK, run)
			return
		}
	case len(rest) == 5 && rest[0] == "runs" && rest[2] == "attempts" && rest[4] == "jobs":
		var jobs []*github.Job
		for _, j := range s.Jobs {
			if fmt.Sprint(j.RunID) == rest[1] && fmt.Sprint(j.RunAttempt) == rest[3] {
				jobs = append(jobs, j)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(jobs), "jobs": jobs})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
