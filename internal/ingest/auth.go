// Package ingest serves gauger's lifecycle and OTLP requests on the runner
// listener.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/mach4-braai/gauger-server/internal/runner"
)

// GitHubIssuer is the issuer of GitHub Actions OIDC tokens.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// TagChecker answers WhoIs for a tailnet peer.
type TagChecker interface {
	HasTag(ctx context.Context, remoteAddr, tag string) (bool, error)
}

// Auth admits a request only when the bearer token is a valid GitHub OIDC
// token for OwnerID and, when Tags is set, the tailnet peer carries Tag.
// Every request is checked on its own, so gauger can switch to a fresh
// token mid-job.
//
// Failures limits failed authentications per client address and answers
// 429 before verifying anything once an address runs out. Jobs limits
// requests per check_run_id claim after authentication.
type Auth struct {
	Tags     TagChecker
	Tag      string
	Verifier *oidc.IDTokenVerifier
	OwnerID  string
	Failures *Limiter
	Jobs     *Limiter
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
		addr := clientAddr(r)
		key := addrKey(addr)
		d, refund := a.Failures.Reserve(key)
		if d > 0 {
			tooMany(w, d)
			return
		}
		if a.Tags != nil {
			ok, err := a.Tags.HasTag(r.Context(), r.RemoteAddr, a.Tag)
			if err != nil || !ok {
				slog.Warn("runner request from an untagged peer", "remote", addr, "err", err)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		raw, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || raw == "" {
			a.unauthorized(w, addr, key, "missing bearer token")
			return
		}
		tok, err := a.Verifier.Verify(r.Context(), raw)
		if err != nil {
			slog.Warn("runner token rejected", "remote", addr, "err", err)
			a.unauthorized(w, addr, key, "invalid token")
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
			slog.Warn("runner token from another owner", "remote", addr, "repository_owner_id", tc.RepositoryOwnerID)
			a.unauthorized(w, addr, key, "invalid token")
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
			slog.Warn("runner token has no job claims", "remote", addr, "repository", tc.Repository,
				"run_id", tc.RunID, "run_attempt", tc.RunAttempt, "check_run_id", tc.CheckRunID)
			a.unauthorized(w, addr, key, "token lacks repository, run_id, run_attempt or check_run_id")
			return
		}
		refund()
		if d := a.Jobs.Take(strconv.FormatInt(c.CheckRunID, 10)); d > 0 {
			slog.Warn("runner job over its request rate", "remote", addr, "check_run_id", c.CheckRunID, "retry_after", d)
			tooMany(w, d)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, c)))
	})
}

// unauthorized answers 401. Wrap already spent one of the address's failed
// authentications for it.
func (a *Auth) unauthorized(w http.ResponseWriter, addr netip.Addr, key, msg string) {
	if d := a.Failures.Wait(key); d > 0 {
		slog.Warn("runner address out of failed authentications", "remote", addr, "key", key, "retry_after", d)
	}
	http.Error(w, msg, http.StatusUnauthorized)
}

func tooMany(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
	http.Error(w, "too many requests", http.StatusTooManyRequests)
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
