// Package ingest serves gauger's lifecycle and OTLP requests on the runner
// listener.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/mach4-braai/gauger-server/internal/runner"
)

// GitHubIssuer is the issuer of GitHub Actions OIDC tokens.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// TagChecker answers WhoIs for a tailnet peer.
type TagChecker interface {
	HasTag(ctx context.Context, remoteAddr, tag string) (bool, error)
}

// Auth admits a request only when the tailnet peer carries Tag and the
// bearer token is a valid GitHub OIDC token for OwnerID. Every request is
// checked on its own, so gauger can switch to a fresh token mid-job.
type Auth struct {
	Tags     TagChecker
	Tag      string
	Verifier *oidc.IDTokenVerifier
	OwnerID  string
}

// Claims name the job a verified token was issued to.
type Claims struct {
	Repository string
	RunID      int64
	RunAttempt int
	CheckRunID int64
}

type claimsKey struct{}

// ClaimsFrom returns the claims Wrap verified for this request.
func ClaimsFrom(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(Claims)
	return c, ok
}

// NewVerifier checks iss, aud and exp against GitHub's published keys.
func NewVerifier(ctx context.Context, audience string) *oidc.IDTokenVerifier {
	keys := oidc.NewRemoteKeySet(ctx, GitHubIssuer+"/.well-known/jwks")
	return oidc.NewVerifier(GitHubIssuer, keys, &oidc.Config{ClientID: audience})
}

func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := a.Tags.HasTag(r.Context(), r.RemoteAddr, a.Tag)
		if err != nil || !ok {
			slog.Warn("runner request from an untagged peer", "remote", r.RemoteAddr, "err", err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		raw, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || raw == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		tok, err := a.Verifier.Verify(r.Context(), raw)
		if err != nil {
			slog.Warn("runner token rejected", "remote", r.RemoteAddr, "err", err)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		var tc struct {
			RepositoryOwnerID string      `json:"repository_owner_id"`
			Repository        string      `json:"repository"`
			RunID             json.Number `json:"run_id"`
			RunAttempt        json.Number `json:"run_attempt"`
			CheckRunID        json.Number `json:"check_run_id"`
		}
		if err := tok.Claims(&tc); err != nil || tc.RepositoryOwnerID != a.OwnerID {
			slog.Warn("runner token from another owner", "remote", r.RemoteAddr, "repository_owner_id", tc.RepositoryOwnerID)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		c := Claims{Repository: tc.Repository}
		c.RunID, err = strconv.ParseInt(tc.RunID.String(), 10, 64)
		if err == nil {
			c.RunAttempt, err = strconv.Atoi(tc.RunAttempt.String())
		}
		if err == nil {
			c.CheckRunID, err = strconv.ParseInt(tc.CheckRunID.String(), 10, 64)
		}
		if err != nil || c.Repository == "" || c.RunID <= 0 || c.RunAttempt <= 0 || c.CheckRunID <= 0 {
			slog.Warn("runner token has no job claims", "remote", r.RemoteAddr, "repository", tc.Repository,
				"run_id", tc.RunID, "run_attempt", tc.RunAttempt, "check_run_id", tc.CheckRunID)
			http.Error(w, "token lacks repository, run_id, run_attempt or check_run_id", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, c)))
	})
}

// bind returns id with CheckRunID taken from the claims when the body left
// it out, or an error when id names a different job than the token.
func (c Claims) bind(id runner.Identity) (runner.Identity, error) {
	if id.CheckRunID == 0 {
		id.CheckRunID = c.CheckRunID
	}
	switch {
	case id.CheckRunID != c.CheckRunID:
		return id, fmt.Errorf("%s %d does not match the token's check_run_id %d", runner.AttrCheckRunID, id.CheckRunID, c.CheckRunID)
	case id.RunID != c.RunID:
		return id, fmt.Errorf("%s %d does not match the token's run_id %d", runner.AttrRunID, id.RunID, c.RunID)
	case id.RunAttempt != c.RunAttempt:
		return id, fmt.Errorf("%s %d does not match the token's run_attempt %d", runner.AttrRunAttempt, id.RunAttempt, c.RunAttempt)
	case !strings.EqualFold(id.Repository, c.Repository):
		return id, fmt.Errorf("%s %s does not match the token's repository %s", runner.AttrRepository, id.Repository, c.Repository)
	}
	return id, nil
}
