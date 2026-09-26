// Package testpg connects integration tests to PGCULPRIT_TEST_DSN and skips them when it's unset.
package testpg

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const envDSN = "PGCULPRIT_TEST_DSN"

func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(envDSN)
	if dsn == "" {
		t.Skip(envDSN + " not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing %s: %v", envDSN, err)
	}
	return connect(t, cfg)
}

func NewDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := Pool(t)
	name := Unique("pgculprit_it")
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = name
	return connect(t, cfg)
}

func Unique(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

func connect(t *testing.T, cfg *pgxpool.Config) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("connecting: %v", err)
	}
	return pool
}
