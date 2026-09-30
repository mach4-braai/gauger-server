package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const partitionPrefix = "samples_p"

// partitionAhead is how many days of future partitions maintenance creates.
const partitionAhead = 2

func partitionName(day time.Time) string {
	return partitionPrefix + day.UTC().Format("20060102")
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// RetentionStart is the earliest timestamp still kept. Samples before it
// live in partitions that maintenance drops.
func (s *Store) RetentionStart(now time.Time) time.Time {
	return dayStart(now.Add(-s.retention))
}

// EnsurePartition creates the daily partition that holds t.
func (s *Store) EnsurePartition(ctx context.Context, t time.Time) error {
	day := dayStart(t)
	name := partitionName(day)
	s.partMu.Lock()
	defer s.partMu.Unlock()
	if s.partitions[name] {
		return nil
	}
	sql := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF samples FOR VALUES FROM ('%s') TO ('%s')`,
		pgx.Identifier{name}.Sanitize(),
		day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339),
	)
	if _, err := s.Pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("create partition %s: %w", name, err)
	}
	s.partitions[name] = true
	return nil
}

// MaintainPartitions creates partitions from yesterday to partitionAhead days
// out, and drops every partition that ends before the retention window.
func (s *Store) MaintainPartitions(ctx context.Context, now time.Time) error {
	for d := -1; d <= partitionAhead; d++ {
		if err := s.EnsurePartition(ctx, now.AddDate(0, 0, d)); err != nil {
			return err
		}
	}
	return s.dropExpired(ctx, now)
}

func (s *Store) dropExpired(ctx context.Context, now time.Time) error {
	rows, err := s.Pool.Query(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'samples'`)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	cutoff := s.RetentionStart(now)
	for _, name := range names {
		day, err := time.Parse("20060102", strings.TrimPrefix(name, partitionPrefix))
		if err != nil || !strings.HasPrefix(name, partitionPrefix) {
			continue
		}
		if !day.AddDate(0, 0, 1).After(cutoff) {
			if _, err := s.Pool.Exec(ctx, `DROP TABLE `+pgx.Identifier{name}.Sanitize()); err != nil {
				return fmt.Errorf("drop partition %s: %w", name, err)
			}
			s.partMu.Lock()
			delete(s.partitions, name)
			s.partMu.Unlock()
			slog.Info("dropped expired sample partition", "partition", name)
		}
	}
	return nil
}

// RunMaintenance repeats MaintainPartitions every hour until ctx ends.
func (s *Store) RunMaintenance(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := s.MaintainPartitions(ctx, now); err != nil {
				slog.Error("partition maintenance", "err", err)
			}
		}
	}
}
