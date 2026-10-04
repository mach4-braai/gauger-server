// Package store owns the Postgres schema and every query gauger-server runs.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mach4-braai/gauger-server/internal/github"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock is the advisory lock key held while migrations run.
const migrationLock = 0x6761756765720001

type Store struct {
	Pool      *pgxpool.Pool
	retention time.Duration

	partMu     sync.Mutex
	partitions map[string]bool

	credMu sync.Mutex
	creds  *github.Credentials

	changeMu sync.Mutex
	changed  chan struct{}
}

// Open connects to Postgres, applies pending migrations and makes sure the
// sample partitions around today exist.
func Open(ctx context.Context, url string, retention time.Duration) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Store{Pool: pool, retention: retention, partitions: map[string]bool{}}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.MaintainPartitions(ctx, time.Now()); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Changed returns a channel that closes after the next committed write
// made through this Store: a transaction from InTx or a statement from
// Exec or one of the write methods. A reader that wants every change takes
// the channel before it reads, so a write that lands during the read closes
// it. Writes made outside this process, such as with psql, don't close it.
func (s *Store) Changed() <-chan struct{} {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.changed
}

func (s *Store) bump() {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

func (s *Store) Retention() time.Duration { return s.retention }

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLock)); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(migrationLock))

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}
	applied := map[int]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return err
	}
	for _, v := range versions {
		applied[int(v)] = true
	}

	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		base := strings.TrimPrefix(f, "migrations/")
		version, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", base)
		}
		if applied[version] {
			continue
		}
		body, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", base, err)
		}
	}
	return nil
}
