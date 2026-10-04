package ui_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/runner"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

type patch struct {
	html string
	at   time.Time
}

// liveStream opens a live stream and delivers each patch of #page until
// the test ends or cancel is called. The client does not reuse connections,
// so closing the stream closes the socket.
func liveStream(t *testing.T, base, path string) (patches <-chan patch, cancel func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET %s: status %d, content type %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan patch, 64)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 4<<20)
		var event string
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if event == "datastar-patch-elements" {
					out <- patch{html: strings.Join(data, "\n"), at: time.Now()}
				}
				event, data = "", nil
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: elements "):
				data = append(data, strings.TrimPrefix(line, "data: elements "))
			}
		}
	}()
	return out, cancel
}

func next(t *testing.T, patches <-chan patch, within time.Duration) (patch, bool) {
	t.Helper()
	select {
	case p, ok := <-patches:
		if !ok {
			t.Fatal("stream closed")
		}
		return p, true
	case <-time.After(within):
		return patch{}, false
	}
}

func runsCard(t *testing.T, page string) int {
	t.Helper()
	v, ok := cards(page)["runs"]
	if !ok {
		t.Fatalf("no runs card in %.300s", page)
	}
	n, err := strconv.Atoi(strings.ReplaceAll(v[0], ",", ""))
	if err != nil {
		t.Fatalf("runs card %q: %v", v[0], err)
	}
	return n
}

func TestLivePatchesAfterRunInsert(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	srv := httptest.NewServer(dashboard(st, fx.Now))
	t.Cleanup(srv.Close)

	patches, _ := liveStream(t, srv.URL, "/stats/?range=7d&live=1")
	first, ok := next(t, patches, 5*time.Second)
	if !ok {
		t.Fatal("no patch when the stream opened")
	}
	before := runsCard(t, first.html)
	if want := runsCard(t, get(t, dashboard(st, fx.Now), "/stats/?range=7d").Body.String()); before != want {
		t.Fatalf("first patch counts %d runs, the page counts %d", before, want)
	}

	started := fx.Now.Add(-time.Hour)
	err := st.InTx(context.Background(), func(tx pgx.Tx) error {
		_, err := store.UpsertRun(context.Background(), tx, "acme/api", &github.Run{
			ID: 9_000_001, RunAttempt: 1, Name: "CI", Event: "push", Status: "in_progress",
			CreatedAt: &started, RunStartedAt: &started,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted := time.Now()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case p, ok := <-patches:
			if !ok {
				t.Fatal("stream closed")
			}
			if got := runsCard(t, p.html); got == before+1 {
				t.Logf("patch with %s runs arrived %v after the insert", chart.Integer(float64(got)), p.at.Sub(inserted).Round(time.Millisecond))
				return
			}
		case <-deadline:
			t.Fatalf("no patch with %d runs within 2s of the insert", before+1)
		}
	}
}

func TestLiveThrottlesABurstOfWrites(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	srv := httptest.NewServer(dashboard(st, fx.Now))
	t.Cleanup(srv.Close)

	patches, _ := liveStream(t, srv.URL, "/stats/?live=1")
	first, ok := next(t, patches, 5*time.Second)
	if !ok {
		t.Fatal("no patch when the stream opened")
	}

	now := time.Now()
	for i := range 50 {
		_, err := st.InsertSamples(context.Background(), fx.SampledJob, []runner.Point{
			{Metric: "cpu", Time: now.Add(-time.Duration(i) * time.Second), Value: float64(i)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var got []patch
	for p, ok := next(t, patches, 3200*time.Millisecond-time.Since(first.at)); ok; p, ok = next(t, patches, 3200*time.Millisecond-time.Since(first.at)) {
		got = append(got, p)
	}
	if len(got) != 1 {
		t.Fatalf("50 writes in a burst made %d patches in 3.2s, want 1", len(got))
	}
	if gap := got[0].at.Sub(first.at); gap < 1300*time.Millisecond {
		t.Fatalf("the patch came %v after the previous one, want about 1.5s", gap)
	}
}

func TestLiveStreamEndsWhenClientCloses(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	srv := httptest.NewServer(dashboard(st, fx.Now))
	t.Cleanup(srv.Close)

	open := func(n int) (cancel func()) {
		var cancels []func()
		for range n {
			patches, c := liveStream(t, srv.URL, "/stats/?live=1")
			if _, ok := next(t, patches, 5*time.Second); !ok {
				t.Fatal("no patch when the stream opened")
			}
			cancels = append(cancels, c)
		}
		return func() {
			for _, c := range cancels {
				c()
			}
		}
	}
	// stable waits until the goroutine count stops moving.
	stable := func() int {
		prev := -1
		for range 100 {
			n := runtime.NumGoroutine()
			if n == prev {
				return n
			}
			prev = n
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("goroutine count never settled")
		return 0
	}

	open(1)()
	baseline := stable()

	closeAll := open(5)
	if n := runtime.NumGoroutine(); n <= baseline {
		t.Fatalf("5 open streams left %d goroutines, baseline %d", n, baseline)
	}
	closeAll()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		buf := make([]byte, 1<<16)
		t.Fatalf("%d goroutines after closing the streams, baseline %d\n%s", n, baseline, buf[:runtime.Stack(buf, true)])
	}
}
