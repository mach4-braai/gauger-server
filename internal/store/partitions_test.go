package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

func partitions(t *testing.T, s *store.Store) []string {
	t.Helper()
	rows, err := s.Pool.Query(context.Background(), `
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'samples' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestRetentionDropsWholeDayPartitions(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t, 3*24*time.Hour)
	now := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC)

	for _, day := range []string{"2026-09-26", "2026-09-27", "2026-09-28"} {
		d, _ := time.Parse(time.DateOnly, day)
		if err := s.EnsurePartition(ctx, d.Add(12*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO samples (job_id, ts, metric, value) VALUES (1, $1, 'm', 1)`, d.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MaintainPartitions(ctx, now); err != nil {
		t.Fatal(err)
	}

	have := map[string]bool{}
	for _, name := range partitions(t, s) {
		have[name] = true
	}
	if have["samples_p20260926"] {
		t.Error("26 Sep ends before the 3-day window and should be dropped")
	}
	for _, name := range []string{"samples_p20260927", "samples_p20260928", "samples_p20260929", "samples_p20260930", "samples_p20261001", "samples_p20261002"} {
		if !have[name] {
			t.Errorf("missing partition %s", name)
		}
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM samples`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("samples left = %d, want 2", n)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	for range 2 {
		s, err := store.Open(ctx, url, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}
