// Package ingest serves gauger's lifecycle and OTLP requests on the runner
// listener.
package ingest

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
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
		var claims struct {
			RepositoryOwnerID string `json:"repository_owner_id"`
		}
		if err := tok.Claims(&claims); err != nil || claims.RepositoryOwnerID != a.OwnerID {
			slog.Warn("runner token from another owner", "remote", r.RemoteAddr, "repository_owner_id", claims.RepositoryOwnerID)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
