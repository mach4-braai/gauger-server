// Package github talks to the GitHub REST API as a GitHub App and checks
// webhook signatures.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

var ErrNotConfigured = errors.New("GitHub App is not configured")

// Credentials identify the GitHub App.
type Credentials struct {
	AppID         int64
	Slug          string
	HTMLURL       string
	PrivateKey    *rsa.PrivateKey
	WebhookSecret string
	FromEnv       bool
}

type CredentialSource interface {
	// Credentials returns ErrNotConfigured until an App exists.
	Credentials(ctx context.Context) (*Credentials, error)
}

// StaticCredentials is a CredentialSource that never changes.
type StaticCredentials struct{ C *Credentials }

func (s StaticCredentials) Credentials(context.Context) (*Credentials, error) { return s.C, nil }

// RateLimitError means GitHub asked us to wait until Until.
type RateLimitError struct{ Until time.Time }

func (e *RateLimitError) Error() string {
	return "GitHub rate limit until " + e.Until.Format(time.RFC3339)
}

type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GitHub returned %d: %s", e.Code, e.Body)
}

func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// HourlyBudget is the request rate each installation may use. GitHub gives
// every installation at least 5,000 requests an hour.
const HourlyBudget = 5000

// maxLocalWait is the longest a request sleeps for the local budget before
// it returns a RateLimitError instead.
const maxLocalWait = 5 * time.Second

type Client struct {
	APIURL string
	HTTP   *http.Client
	Creds  CredentialSource

	mu     sync.Mutex
	tokens map[int64]token
	limits map[int64]*limit
	appJWT token
}

type token struct {
	value   string
	expires time.Time
}

type limit struct {
	budget *rate.Limiter
	until  time.Time
}

func NewClient(apiURL string, creds CredentialSource) *Client {
	return &Client{
		APIURL: strings.TrimSuffix(apiURL, "/"),
		HTTP:   &http.Client{Timeout: 30 * time.Second},
		Creds:  creds,
		tokens: map[int64]token{},
		limits: map[int64]*limit{},
	}
}

func (c *Client) limitFor(inst int64) *limit {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.limits[inst]
	if !ok {
		l = &limit{budget: rate.NewLimiter(rate.Every(time.Hour/HourlyBudget), 100)}
		c.limits[inst] = l
	}
	return l
}

// wait blocks for the local budget, or returns a RateLimitError when GitHub
// told us to back off or the budget would make us wait too long.
func (c *Client) wait(ctx context.Context, inst int64) error {
	l := c.limitFor(inst)
	now := time.Now()
	c.mu.Lock()
	until := l.until
	c.mu.Unlock()
	if until.After(now) {
		return &RateLimitError{Until: until}
	}
	res := l.budget.ReserveN(now, 1)
	delay := res.DelayFrom(now)
	if delay > maxLocalWait {
		res.CancelAt(now)
		return &RateLimitError{Until: now.Add(delay)}
	}
	if delay == 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		res.Cancel()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// observe records x-ratelimit-* and retry-after, and returns a
// RateLimitError when the response is a rate-limit rejection.
func (c *Client) observe(inst int64, resp *http.Response, body []byte) error {
	now := time.Now()
	var until time.Time
	remaining := resp.Header.Get("X-Ratelimit-Remaining")
	if remaining == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			until = time.Unix(reset, 0)
		}
	}
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (remaining == "0" || resp.Header.Get("Retry-After") != "" ||
			bytes.Contains(bytes.ToLower(body), []byte("rate limit"))))
	if limited {
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			until = now.Add(time.Duration(secs) * time.Second)
		} else if until.IsZero() {
			until = now.Add(time.Minute)
		}
	}
	if until.IsZero() {
		return nil
	}
	l := c.limitFor(inst)
	c.mu.Lock()
	if until.After(l.until) {
		l.until = until
	}
	c.mu.Unlock()
	if limited {
		return &RateLimitError{Until: until}
	}
	return nil
}

func (c *Client) creds(ctx context.Context) (*Credentials, error) {
	if c.Creds == nil {
		return nil, ErrNotConfigured
	}
	return c.Creds.Credentials(ctx)
}

func (c *Client) jwt(ctx context.Context) (string, error) {
	cr, err := c.creds(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.appJWT.value != "" && now.Before(c.appJWT.expires) {
		return c.appJWT.value, nil
	}
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    strconv.FormatInt(cr.AppID, 10),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(cr.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	c.appJWT = token{value: signed, expires: now.Add(8 * time.Minute)}
	return signed, nil
}

func (c *Client) installationToken(ctx context.Context, inst int64) (string, error) {
	c.mu.Lock()
	t, ok := c.tokens[inst]
	c.mu.Unlock()
	if ok && time.Now().Before(t.expires) {
		return t.value, nil
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.do(ctx, 0, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst), nil, &out); err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	c.mu.Lock()
	c.tokens[inst] = token{value: out.Token, expires: out.ExpiresAt.Add(-5 * time.Minute)}
	c.mu.Unlock()
	return out.Token, nil
}

// do sends one request. inst 0 authenticates as the App with a JWT, anything
// else as that installation.
func (c *Client) do(ctx context.Context, inst int64, method, path string, body any, out any) error {
	resp, data, err := c.send(ctx, inst, method, c.APIURL+path, body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &StatusError{Code: resp.StatusCode, Body: truncate(data)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) send(ctx context.Context, inst int64, method, u string, body any) (*http.Response, []byte, error) {
	if err := c.wait(ctx, inst); err != nil {
		return nil, nil, err
	}
	var auth string
	if inst == 0 {
		j, err := c.jwt(ctx)
		if err != nil {
			return nil, nil, err
		}
		auth = "Bearer " + j
	} else {
		t, err := c.installationToken(ctx, inst)
		if err != nil {
			return nil, nil, err
		}
		auth = "token " + t
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gauger-server")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return nil, nil, err
	}
	if err := c.observe(inst, resp, data); err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && inst != 0 {
		c.mu.Lock()
		delete(c.tokens, inst)
		c.mu.Unlock()
	}
	return resp, data, nil
}

func truncate(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}

func splitRepo(full string) (string, string, error) {
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository %q is not owner/name", full)
	}
	return url.PathEscape(owner), url.PathEscape(name), nil
}

// RepoInstallation returns the App installation that covers repo.
func (c *Client) RepoInstallation(ctx context.Context, repo string) (int64, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, err
	}
	var out Installation
	if err := c.do(ctx, 0, http.MethodGet, fmt.Sprintf("/repos/%s/%s/installation", owner, name), nil, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// MaxRunsPerQuery is the most runs GitHub returns for one created query,
// regardless of what total_count reports.
const MaxRunsPerQuery = 1000

// ListRuns returns the workflow runs created in [from, to], GitHub's
// inclusive date range, and the query's total_count. GitHub never returns
// more than MaxRunsPerQuery runs for one query, however large total_count
// is. When stopOverCap is true, ListRuns stops after the first page once it
// sees total_count is over MaxRunsPerQuery, since a caller narrowing the
// range would throw the rest away; pass false to page all the way to
// MaxRunsPerQuery when the range can't be narrowed any further.
func (c *Client) ListRuns(ctx context.Context, inst int64, repo string, from, to time.Time, stopOverCap bool) ([]Run, int, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, 0, err
	}
	created := from.UTC().Format("2006-01-02") + ".." + to.UTC().Format("2006-01-02")
	var runs []Run
	total := 0
	for page := 1; ; page++ {
		var out struct {
			TotalCount   int   `json:"total_count"`
			WorkflowRuns []Run `json:"workflow_runs"`
		}
		path := fmt.Sprintf("/repos/%s/%s/actions/runs?created=%s&per_page=100&page=%d", owner, name, url.QueryEscape(created), page)
		if err := c.do(ctx, inst, http.MethodGet, path, nil, &out); err != nil {
			return nil, 0, err
		}
		if page == 1 {
			total = out.TotalCount
		}
		runs = append(runs, out.WorkflowRuns...)
		if len(out.WorkflowRuns) < 100 || len(runs) >= total || len(runs) >= MaxRunsPerQuery ||
			(stopOverCap && total > MaxRunsPerQuery) {
			return runs, total, nil
		}
	}
}

func (c *Client) GetJob(ctx context.Context, inst int64, repo string, id int64) (*Job, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out Job
	if err := c.do(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/jobs/%d", owner, name, id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetRunAttempt(ctx context.Context, inst int64, repo string, runID int64, attempt int) (*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out Run
	if err := c.do(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/runs/%d/attempts/%d", owner, name, runID, attempt), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListRunAttemptJobs(ctx context.Context, inst int64, repo string, runID int64, attempt int) ([]Job, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for page := 1; ; page++ {
		var out struct {
			TotalCount int   `json:"total_count"`
			Jobs       []Job `json:"jobs"`
		}
		path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/attempts/%d/jobs?per_page=100&page=%d", owner, name, runID, attempt, page)
		if err := c.do(ctx, inst, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		jobs = append(jobs, out.Jobs...)
		if len(out.Jobs) < 100 || len(jobs) >= out.TotalCount {
			return jobs, nil
		}
	}
}

// ListRunArtifacts returns the run's artifacts called name, or all of them
// when name is empty.
func (c *Client) ListRunArtifacts(ctx context.Context, inst int64, repo string, runID int64, name string) ([]Artifact, error) {
	owner, repoName, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	filter := ""
	if name != "" {
		filter = "&name=" + url.QueryEscape(name)
	}
	var arts []Artifact
	for page := 1; ; page++ {
		var out struct {
			TotalCount int        `json:"total_count"`
			Artifacts  []Artifact `json:"artifacts"`
		}
		path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/artifacts?per_page=100&page=%d%s", owner, repoName, runID, page, filter)
		if err := c.do(ctx, inst, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		arts = append(arts, out.Artifacts...)
		if len(out.Artifacts) < 100 || len(arts) >= out.TotalCount {
			return arts, nil
		}
	}
}

// DownloadArtifact returns the artifact's zip. GitHub redirects to blob
// storage; net/http drops the Authorization header on that redirect.
func (c *Client) DownloadArtifact(ctx context.Context, inst int64, repo string, id int64) ([]byte, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	resp, data, err := c.send(ctx, inst, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/actions/artifacts/%d/zip", c.APIURL, owner, name, id), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Code: resp.StatusCode, Body: truncate(data)}
	}
	return data, nil
}
