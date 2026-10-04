package storetest

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TaskLastError is the error of the seeded "run" task that has failed.
const TaskLastError = "github: 502 bad gateway fetching run 1000042"

// health adds 40 days of webhook deliveries, so the oldest ten fall
// outside the 30 days the server keeps, and a task queue with due,
// future and failing tasks.
func (s *seeder) health() {
	events := []struct {
		name  string
		every int
	}{{"workflow_job", 1}, {"workflow_run", 2}, {"push", 3}, {"pull_request", 5}, {"check_run", 7}, {"ping", 40}}

	var deliveries [][]any
	for day := 39; day >= 0; day-- {
		for i, e := range events {
			if day%e.every != 0 {
				continue
			}
			for n := range 1 + (day+i)%3 {
				at := s.now.Add(-time.Duration(day)*24*time.Hour - time.Duration(1+i*37+n*11)*time.Minute)
				deliveries = append(deliveries, []any{fmt.Sprintf("seed-%02d-%s-%d", day, e.name, n), e.name, at})
			}
		}
	}

	type task struct {
		kind, key, repo string
		attempts        int
		next            time.Time
		lastError       any
	}
	tasks := []task{
		{"run", "1000042:1", "acme/api", 3, s.now.Add(10 * time.Minute), TaskLastError},
		{"run", "1000043:1", "acme/api", 0, s.now.Add(-90 * time.Second), nil},
		{"run", "1000044:2", "oss/gauger", 1, s.now.Add(-4 * time.Minute), "github: 403 rate limited"},
		{"job", "50000001", "oss/gauger", 0, s.now.Add(30 * time.Second), nil},
		{"artifact", "50000002", "oss/gauger", 0, s.now.Add(2 * time.Minute), nil},
	}
	for i := range 6 {
		tasks = append(tasks, task{"backfill", fmt.Sprintf("acme/web:2026-07-0%d", i+1), "acme/web", 0, s.now.Add(-time.Duration(i+1) * time.Hour), nil})
	}

	s.inserts = append(s.inserts, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"webhook_deliveries"}, []string{"id", "event", "received_at"}, pgx.CopyFromRows(deliveries)); err != nil {
			return fmt.Errorf("webhook_deliveries: %w", err)
		}
		for _, t := range tasks {
			_, err := tx.Exec(ctx, `
				INSERT INTO tasks (kind, key, repository, attempts, next_at, expires_at, last_error)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				t.kind, t.key, t.repo, t.attempts, t.next, s.now.Add(7*24*time.Hour), t.lastError)
			if err != nil {
				return fmt.Errorf("tasks: %w", err)
			}
		}
		return nil
	})
}
