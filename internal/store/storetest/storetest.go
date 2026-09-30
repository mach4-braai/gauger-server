// Package storetest opens a throwaway Postgres database per test.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/store"
)

// URL creates an empty database and returns its URL. The test is skipped
// when GAUGER_TEST_DATABASE_URL is unset.
func URL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv("GAUGER_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("GAUGER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}
	defer conn.Close(ctx)

	b := make([]byte, 6)
	rand.Read(b)
	name := "gauger_test_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// Open returns a migrated store on a fresh database.
func Open(t testing.TB, retention time.Duration) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), URL(t), retention)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}
