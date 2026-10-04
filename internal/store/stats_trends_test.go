package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

func TestStepDailyMediansPerUTCDay(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	ctx := context.Background()
	today := fx.Now.Truncate(24 * time.Hour)
	const step = "Run mise run bench"

	days, err := st.StepDaily(ctx, today.AddDate(0, 0, -30), "acme/api", "Bench", "bench", step, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 16 {
		t.Fatalf("got %d days, want the 16 the fixtures run on", len(days))
	}
	for i, d := range days {
		ago := 16 - i
		want := 60.0
		if ago == 1 {
			want = 150
		}
		if !d.Day.Equal(today.AddDate(0, 0, -ago)) || d.Median != want || d.Runs != 2 {
			t.Errorf("day %d = %+v, want %v with median %v and 2 runs", i, d, today.AddDate(0, 0, -ago), want)
		}
	}

	since, err := st.StepDaily(ctx, today.AddDate(0, 0, -3), "acme/api", "Bench", "bench", step, "main")
	if err != nil || len(since) != 3 {
		t.Errorf("from three days back got %d days, %v; want 3", len(since), err)
	}
	for _, other := range [][5]string{
		{"acme/web", "Bench", "bench", step, "main"},
		{"acme/api", "Bench", "bench", step, "dev"},
		{"acme/api", "Bench", "bench", "Complete job", "dev"},
		{"acme/api", "CI", "bench", step, "main"},
	} {
		got, err := st.StepDaily(ctx, today.AddDate(0, 0, -30), other[0], other[1], other[2], other[3], other[4])
		if err != nil || len(got) != 0 {
			t.Errorf("StepDaily(%v) = %v, %v; want nothing", other, got, err)
		}
	}
}
