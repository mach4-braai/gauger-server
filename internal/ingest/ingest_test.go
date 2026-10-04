package ingest_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	metrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/github/githubtest"
	"github.com/mach4-braai/gauger-server/internal/ingest"
	"github.com/mach4-braai/gauger-server/internal/reconcile"
	"github.com/mach4-braai/gauger-server/internal/runner"
	"github.com/mach4-braai/gauger-server/internal/server"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

const (
	ownerID     = "287937105"
	ciPeer      = "100.64.0.10:40000"
	otherCIPeer = "100.64.0.11:40000"
	userPeer    = "100.64.0.20:40000"
)

type tags map[string][]string

func (t tags) HasTag(_ context.Context, remote, tag string) (bool, error) {
	for _, have := range t[remote] {
		if have == tag {
			return true, nil
		}
	}
	return false, nil
}

type oidcIssuer struct{ key *rsa.PrivateKey }

func newIssuer(t *testing.T) *oidcIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &oidcIssuer{key: key}
}

func (i *oidcIssuer) verifier() *oidc.IDTokenVerifier {
	keys := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&i.key.PublicKey}}
	return oidc.NewVerifier(ingest.GitHubIssuer, keys, &oidc.Config{ClientID: "gauger-server"})
}

func (i *oidcIssuer) token(t *testing.T, edit func(jwt.MapClaims)) string {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": ingest.GitHubIssuer, "aud": "gauger-server", "sub": "repo:acme/app:ref:refs/heads/main",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"repository_owner_id": ownerID, "repository": "acme/app",
		"run_id": "100", "run_attempt": "1", "check_run_id": "555",
	}
	if edit != nil {
		edit(claims)
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(i.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// jobToken is a token issued to job checkRunID of run 100, attempt 1.
func (i *oidcIssuer) jobToken(t *testing.T, checkRunID int64) string {
	return i.token(t, func(c jwt.MapClaims) { c["check_run_id"] = strconv.FormatInt(checkRunID, 10) })
}

type env struct {
	st    *store.Store
	gh    *githubtest.Server
	rec   *reconcile.Reconciler
	iss   *oidcIssuer
	h     http.Handler
	dbURL string
}

func newEnv(t *testing.T) *env {
	url := storetest.URL(t)
	e := &env{gh: githubtest.New(t), iss: newIssuer(t), dbURL: url}
	e.start(t)
	return e
}

// start builds a fresh server process on the same database, as a restart does.
func (e *env) start(t *testing.T) {
	st, err := store.Open(context.Background(), e.dbURL, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	e.st = st
	e.rec = reconcile.New(st, github.NewClient(e.gh.URL, github.StaticCredentials{C: e.gh.Credentials()}))
	auth := &ingest.Auth{
		Tags: tags{ciPeer: {"tag:gauger-ci"}, otherCIPeer: {"tag:gauger-ci"}}, Tag: "tag:gauger-ci",
		Verifier: e.iss.verifier(), OwnerID: ownerID,
		Failures: ingest.FailedAuthLimiter(), Jobs: ingest.JobLimiter(),
	}
	e.h = auth.Wrap((&ingest.Handler{Store: st, Reconciler: e.rec}).Routes())
}

func (e *env) do(t *testing.T, path, contentType string, body []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	return e.doFrom(t, ciPeer, path, contentType, body, token)
}

func (e *env) doFrom(t *testing.T, remote, path, contentType string, body []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// lifecycle posts id with a token issued to the job id names.
func (e *env) lifecycle(t *testing.T, path string, id map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(id)
	checkRunID, _ := id[runner.AttrCheckRunID].(int64)
	return e.do(t, path, "application/json", b, e.iss.jobToken(t, checkRunID))
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// makeDue pulls every task forward so ProcessDue runs it now.
func (e *env) makeDue(t *testing.T) {
	if _, err := e.st.Pool.Exec(context.Background(), `UPDATE tasks SET next_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
}

func identity(checkRunID int64) map[string]any {
	id := map[string]any{
		runner.AttrRunID: 100, runner.AttrRunAttempt: 1, runner.AttrRepository: "acme/app",
		runner.AttrWorkflow: "CI", runner.AttrJob: "build", runner.AttrRunnerName: "GitHub Actions 3",
	}
	if checkRunID != 0 {
		id[runner.AttrCheckRunID] = checkRunID
	}
	return id
}

func batch(checkRunID int64, at time.Time, values ...float64) []byte {
	res := []*common.KeyValue{}
	for k, v := range identity(checkRunID) {
		var av *common.AnyValue
		switch x := v.(type) {
		case string:
			av = &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: x}}
		case int:
			av = &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: int64(x)}}
		case int64:
			av = &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: x}}
		}
		res = append(res, &common.KeyValue{Key: k, Value: av})
	}
	var dps []*metrics.NumberDataPoint
	for i, v := range values {
		dps = append(dps, &metrics.NumberDataPoint{
			TimeUnixNano: uint64(at.Add(time.Duration(i) * time.Second).UnixNano()),
			Value:        &metrics.NumberDataPoint_AsDouble{AsDouble: v},
		})
	}
	req := &colmetrics.ExportMetricsServiceRequest{ResourceMetrics: []*metrics.ResourceMetrics{{
		Resource: &resource.Resource{Attributes: res},
		ScopeMetrics: []*metrics.ScopeMetrics{{Metrics: []*metrics.Metric{{
			Name: "system.cpu.utilization", Data: &metrics.Metric_Gauge{Gauge: &metrics.Gauge{DataPoints: dps}},
		}}}},
	}}}
	b, _ := proto.Marshal(req)
	return b
}

func completedJob(id int64, started, completed time.Time) *github.Job {
	return &github.Job{
		ID: id, RunID: 100, RunAttempt: 1, Name: "build", Status: "completed", Conclusion: "success",
		RunnerName: "GitHub Actions 3", StartedAt: &started, CompletedAt: &completed,
		Steps: []github.Step{
			{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: &started, CompletedAt: &started},
			{Number: 2, Name: "make", Status: "completed", Conclusion: "success", StartedAt: &started, CompletedAt: &completed},
		},
	}
}

func TestAuthNeedsTagAndValidToken(t *testing.T) {
	e := newEnv(t)
	body, _ := json.Marshal(identity(555))
	send := func(remote, token string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs/start", bytes.NewReader(body))
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code
	}
	valid := e.iss.token(t, nil)
	for _, tc := range []struct {
		name   string
		remote string
		token  string
		want   int
	}{
		{"untagged peer with a valid token", userPeer, valid, http.StatusForbidden},
		{"no token", ciPeer, "", http.StatusUnauthorized},
		{"wrong audience", ciPeer, e.iss.token(t, func(c jwt.MapClaims) { c["aud"] = "sts.amazonaws.com" }), http.StatusUnauthorized},
		{"wrong issuer", ciPeer, e.iss.token(t, func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }), http.StatusUnauthorized},
		{"expired", ciPeer, e.iss.token(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Second).Unix() }), http.StatusUnauthorized},
		{"other owner", ciPeer, e.iss.token(t, func(c jwt.MapClaims) { c["repository_owner_id"] = "1" }), http.StatusUnauthorized},
		{"no check_run_id claim", ciPeer, e.iss.token(t, func(c jwt.MapClaims) { delete(c, "check_run_id") }), http.StatusUnauthorized},
		{"valid", ciPeer, valid, http.StatusOK},
		{"fresh token mid-job", ciPeer, e.iss.token(t, func(c jwt.MapClaims) { c["iat"] = time.Now().Unix() + 1 }), http.StatusOK},
	} {
		if got := send(tc.remote, tc.token); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRestartMidJobLosesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	started := time.Now().Add(-10 * time.Minute).Truncate(time.Second)

	if rec := e.lifecycle(t, "/v1/jobs/start", identity(555)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"job_id":555`) {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE id = 555 AND status = 'pending'`); n != 1 {
		t.Fatal("start should store the job as pending")
	}
	if rec := e.do(t, "/v1/metrics", "application/x-protobuf", batch(555, started, 0.1, 0.2, 0.3), e.iss.token(t, nil)); rec.Code != http.StatusOK {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Body)
	}

	e.start(t)

	e.gh.Mu.Lock()
	e.gh.Jobs[555] = &github.Job{ID: 555, RunID: 100, RunAttempt: 1, Status: "in_progress", RunnerName: "GitHub Actions 3", StartedAt: &started}
	e.gh.Mu.Unlock()
	e.makeDue(t)
	e.rec.ProcessDue(ctx)
	if n := e.count(t, `SELECT count(*) FROM tasks WHERE kind = 'job' AND key = '555' AND attempts = 1`); n != 1 {
		t.Fatal("an in-progress job should stay queued for another poll")
	}

	replayed := batch(555, started, 0.1, 0.2, 0.3)
	if rec := e.do(t, "/v1/metrics", "application/x-protobuf", replayed, e.iss.token(t, nil)); rec.Code != http.StatusOK {
		t.Fatalf("replay: %d", rec.Code)
	}
	later := batch(555, started.Add(time.Minute), 0.9)
	e.do(t, "/v1/metrics", "application/x-protobuf", later, e.iss.token(t, nil))
	if rec := e.lifecycle(t, "/v1/jobs/done", identity(555)); rec.Code != http.StatusOK {
		t.Fatalf("done: %d", rec.Code)
	}

	e.gh.Mu.Lock()
	e.gh.Jobs[555] = completedJob(555, started, started.Add(5*time.Minute))
	e.gh.Mu.Unlock()
	e.makeDue(t)
	e.rec.ProcessDue(ctx)

	if n := e.count(t, `SELECT count(*) FROM samples WHERE job_id = 555`); n != 4 {
		t.Fatalf("samples = %d, want 4 with the replay ignored", n)
	}
	if n := e.count(t, `SELECT count(*) FROM steps WHERE job_id = 555 AND completed_at IS NOT NULL`); n != 2 {
		t.Fatalf("steps = %d, want 2 from REST", n)
	}
	if n := e.count(t, `SELECT count(*) FROM tasks`); n != 0 {
		t.Fatalf("tasks left = %d, want none once the job completed and gauger said done", n)
	}
}

func TestFallbackArtifactFillsMissingSamples(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	started := time.Now().Add(-20 * time.Minute).Truncate(time.Second)

	e.lifecycle(t, "/v1/jobs/start", identity(777))
	e.gh.Mu.Lock()
	e.gh.Jobs[777] = completedJob(777, started, started.Add(10*time.Minute))
	e.gh.Mu.Unlock()
	e.makeDue(t)
	e.rec.ProcessDue(ctx)

	var next time.Time
	if err := e.st.Pool.QueryRow(ctx, `SELECT next_at FROM tasks WHERE kind = 'artifact' AND key = '777'`).Scan(&next); err != nil {
		t.Fatalf("no artifact search after the job completed without done: %v", err)
	}
	if want := started.Add(10*time.Minute + store.ArtifactGrace); !next.Equal(want) {
		t.Fatalf("artifact search at %v, want %v", next, want)
	}

	e.rec.ProcessDue(ctx)
	if n := e.count(t, `SELECT count(*) FROM tasks WHERE kind = 'artifact' AND attempts = 1`); n != 1 {
		t.Fatal("a missing artifact should be looked for again later")
	}

	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	for i, b := range [][]byte{batch(777, started, 0.4, 0.5), batch(777, started.Add(time.Minute), 0.6)} {
		f, _ := zw.Create("batch-" + string(rune('0'+i)) + ".pb")
		f.Write(b)
	}
	zw.Close()
	e.gh.Mu.Lock()
	e.gh.Artifacts[runner.ArtifactName(777)] = github.Artifact{ID: 9001, Name: runner.ArtifactName(777)}
	e.gh.Zips[9001] = zipped.Bytes()
	e.gh.Mu.Unlock()
	e.makeDue(t)
	e.rec.ProcessDue(ctx)

	if n := e.count(t, `SELECT count(*) FROM samples WHERE job_id = 777`); n != 3 {
		t.Fatalf("samples = %d, want 3 from the artifact", n)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE id = 777 AND artifact_ingested_at IS NOT NULL`); n != 1 {
		t.Fatal("artifact ingestion should be recorded on the job")
	}
	if n := e.count(t, `SELECT count(*) FROM tasks`); n != 0 {
		t.Fatalf("tasks left = %d", n)
	}
}

func TestCompletedRunKeepsSearchingForArtifactsOfJobsGaugerNeverReached(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	started := time.Now().Add(-20 * time.Minute).Truncate(time.Second)
	expired := time.Now().Add(-8 * 24 * time.Hour)

	e.gh.Mu.Lock()
	e.gh.Runs["100:1"] = &github.Run{ID: 100, RunAttempt: 1, Status: "completed", Conclusion: "success", Repository: github.Repository{FullName: "acme/app"}}
	e.gh.Jobs[777] = completedJob(777, started, started.Add(10*time.Minute))
	e.gh.Jobs[778] = completedJob(778, started, started.Add(10*time.Minute))
	e.gh.Jobs[779] = completedJob(779, expired, expired.Add(10*time.Minute))
	e.gh.Artifacts["coverage"] = github.Artifact{ID: 9002, Name: "coverage"}
	e.gh.Mu.Unlock()
	if err := store.EnqueueTask(ctx, e.st.Pool, store.KindRun, store.RunKey(100, 1), "acme/app", time.Now().Add(-time.Second), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	e.rec.ProcessDue(ctx)
	if n := e.count(t, `SELECT count(*) FROM tasks WHERE kind = 'artifact' AND key IN ('777', '778')`); n != 2 {
		t.Fatalf("artifact searches = %d, want one per completed job before any artifact is listed", n)
	}
	if n := e.count(t, `SELECT count(*) FROM tasks WHERE kind = 'artifact' AND expires_at = $1`, started.Add(10*time.Minute+7*24*time.Hour)); n != 2 {
		t.Fatal("each search should stop 7 days after its job completed")
	}
	if n := e.count(t, `SELECT count(*) FROM tasks WHERE key = '779'`); n != 0 {
		t.Fatal("a job whose artifact has expired should not be searched")
	}
	e.rec.ProcessDue(ctx)
	if n := e.count(t, `SELECT count(*) FROM tasks WHERE kind = 'artifact' AND attempts = 1`); n != 2 {
		t.Fatal("both searches should stay queued while no gauger artifact is listed")
	}

	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	f, _ := zw.Create("0.pb")
	f.Write(batch(777, started, 0.4, 0.5, 0.6))
	zw.Close()
	e.gh.Mu.Lock()
	e.gh.Artifacts[runner.ArtifactName(777)] = github.Artifact{ID: 9001, Name: runner.ArtifactName(777)}
	e.gh.Zips[9001] = zipped.Bytes()
	e.gh.Mu.Unlock()
	e.makeDue(t)
	e.rec.ProcessDue(ctx)

	if n := e.count(t, `SELECT count(*) FROM samples WHERE job_id = 777`); n != 3 {
		t.Fatalf("samples = %d, want 3 from the artifact", n)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE id = 777 AND runner_seen_at IS NULL AND artifact_ingested_at IS NOT NULL`); n != 1 {
		t.Fatal("artifact ingestion should be recorded on a job gauger never reached")
	}
	if n := e.count(t, `SELECT count(*) FROM tasks WHERE kind = 'artifact' AND key = '778' AND attempts = 2`); n != 1 {
		t.Fatal("the job without an artifact should keep searching")
	}
	if n := e.count(t, `SELECT count(*) FROM tasks`); n != 1 {
		t.Fatalf("tasks left = %d, want only the search for 778", n)
	}
}

func TestTokenBindsRequestsToItsJob(t *testing.T) {
	e := newEnv(t)
	tokenA := e.iss.jobToken(t, 555)
	now := time.Now().Truncate(time.Second)
	with := func(k string, v any) map[string]any {
		id := identity(555)
		id[k] = v
		return id
	}

	for _, path := range []string{"/v1/jobs/start", "/v1/jobs/done"} {
		for name, id := range map[string]map[string]any{
			"another job":        identity(556),
			"another run":        with(runner.AttrRunID, 101),
			"another attempt":    with(runner.AttrRunAttempt, 2),
			"another repository": with(runner.AttrRepository, "acme/other"),
		} {
			b, _ := json.Marshal(id)
			if got := e.do(t, path, "application/json", b, tokenA).Code; got != http.StatusForbidden {
				t.Errorf("%s for %s: status %d, want 403", path, name, got)
			}
		}
	}
	if rec := e.do(t, "/v1/metrics", "application/x-protobuf", batch(556, now, 0.1), tokenA); rec.Code != http.StatusForbidden {
		t.Errorf("batch for another job: status %d, want 403", rec.Code)
	}

	var mixed colmetrics.ExportMetricsServiceRequest
	for _, b := range [][]byte{batch(555, now, 0.2), batch(556, now, 0.3)} {
		var part colmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(b, &part); err != nil {
			t.Fatal(err)
		}
		mixed.ResourceMetrics = append(mixed.ResourceMetrics, part.ResourceMetrics...)
	}
	body, _ := proto.Marshal(&mixed)
	if rec := e.do(t, "/v1/metrics", "application/x-protobuf", body, tokenA); rec.Code != http.StatusForbidden {
		t.Errorf("batch with its own and another job: status %d, want 403", rec.Code)
	}
	if n := e.count(t, `SELECT (SELECT count(*) FROM jobs) + (SELECT count(*) FROM samples)`); n != 0 {
		t.Fatalf("rejected requests stored %d rows, want none", n)
	}

	if rec := e.do(t, "/v1/metrics", "application/x-protobuf", batch(0, now, 0.4, 0.5), tokenA); rec.Code != http.StatusOK {
		t.Fatalf("batch without check_run_id: %d %s", rec.Code, rec.Body)
	}
	if n := e.count(t, `SELECT count(*) FROM samples WHERE job_id = 555`); n != 2 {
		t.Fatalf("samples on the token's job = %d, want 2", n)
	}
	b, _ := json.Marshal(identity(0))
	if rec := e.do(t, "/v1/jobs/done", "application/json", b, tokenA); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"job_id":555`) {
		t.Fatalf("done without check_run_id: %d %s, want job 555", rec.Code, rec.Body)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE id = 556`); n != 0 {
		t.Fatal("job 556 should have no rows")
	}
}

func TestJobOverItsRateGets429(t *testing.T) {
	e := newEnv(t)
	tokenA := e.iss.jobToken(t, 555)
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Second)

	if rec := e.lifecycle(t, "/v1/jobs/start", identity(555)); rec.Code != http.StatusOK {
		t.Fatalf("start: %d", rec.Code)
	}
	for i := range 60 {
		if rec := e.do(t, "/v1/metrics", "application/x-protobuf", batch(555, start.Add(time.Duration(i)*5*time.Second), 0.5), tokenA); rec.Code != http.StatusOK {
			t.Fatalf("replayed batch %d of 60: %d %s", i+1, rec.Code, rec.Body)
		}
	}

	var limited *httptest.ResponseRecorder
	for range 200 {
		rec := e.do(t, "/v1/metrics", "application/x-protobuf", batch(555, start, 0.5), tokenA)
		if rec.Code == http.StatusTooManyRequests {
			limited = rec
			break
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d before the limit", rec.Code)
		}
	}
	if limited == nil || limited.Header().Get("Retry-After") == "" {
		t.Fatal("a job sending without pause should get 429 with Retry-After")
	}
	if rec := e.do(t, "/v1/metrics", "application/x-protobuf", batch(556, start, 0.5), e.iss.jobToken(t, 556)); rec.Code != http.StatusOK {
		t.Fatalf("another job while 555 is limited: %d", rec.Code)
	}
}

func TestRepeatedBadTokensFromOneAddressGet429(t *testing.T) {
	e := newEnv(t)
	body, _ := json.Marshal(identity(555))
	bad := e.iss.token(t, func(c jwt.MapClaims) { c["aud"] = "sts.amazonaws.com" })

	var limited *httptest.ResponseRecorder
	for range 50 {
		rec := e.doFrom(t, ciPeer, "/v1/jobs/start", "application/json", body, bad)
		if rec.Code == http.StatusTooManyRequests {
			limited = rec
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad token: status %d, want 401 until the limit", rec.Code)
		}
	}
	if limited == nil || limited.Header().Get("Retry-After") == "" {
		t.Fatal("repeated bad tokens should get 429 with Retry-After")
	}
	if rec := e.doFrom(t, ciPeer, "/v1/jobs/start", "application/json", body, e.iss.token(t, nil)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("valid token from the limited address: %d, want 429 before verification", rec.Code)
	}
	if rec := e.doFrom(t, otherCIPeer, "/v1/jobs/start", "application/json", body, bad); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token from another address: %d, want 401", rec.Code)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("jobs = %d, want none", n)
	}
}

func TestFunnelRunnerNeedsOnlyAValidToken(t *testing.T) {
	e := newEnv(t)
	auth := &ingest.Auth{Verifier: e.iss.verifier(), OwnerID: ownerID, Failures: ingest.FailedAuthLimiter(), Jobs: ingest.JobLimiter()}
	h := (&server.Server{FunnelRunner: auth.Wrap((&ingest.Handler{Store: e.st, Reconciler: e.rec}).Routes())}).FunnelRunnerHandler()
	const relay = "100.100.100.100:443"
	send := func(method, path, client string, body []byte, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req = req.WithContext(ingest.WithClientAddr(req.Context(), netip.MustParseAddr(client)))
		req.RemoteAddr = relay
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	start, _ := json.Marshal(identity(555))

	if rec := send(http.MethodGet, "/healthz", "203.0.113.7", nil, ""); rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body)
	}
	if rec := send(http.MethodPost, "/v1/metrics", "203.0.113.7", make([]byte, 20<<20), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("20 MiB body without a token: %d, want 401", rec.Code)
	}
	if rec := send(http.MethodPost, "/v1/jobs/start", "203.0.113.7", start, e.iss.token(t, nil)); rec.Code != http.StatusOK {
		t.Fatalf("valid token from an untagged internet client: %d %s", rec.Code, rec.Body)
	}

	bad := e.iss.token(t, func(c jwt.MapClaims) { c["aud"] = "sts.amazonaws.com" })
	limited := false
	for range 50 {
		if send(http.MethodPost, "/v1/jobs/start", "203.0.113.7", start, bad).Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated bad tokens from one internet client should get 429")
	}
	if rec := send(http.MethodPost, "/v1/jobs/start", "198.51.100.9", start, bad); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another client behind the same relay: %d, want 401", rec.Code)
	}
}
