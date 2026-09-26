package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Stops two collectors migrating the same store at once.
const migrationLock = 0x7067626c616d65 // "pgblame"

func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLock)); err != nil {
		return fmt.Errorf("taking migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(migrationLock))

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS pgblame_schema_migrations (
		version    integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}

	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, "migrations/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s has no numeric prefix", name)
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			var applied bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pgblame_schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil || applied {
				return err
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO pgblame_schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return fmt.Errorf("applying %s: %w", name, err)
		}
	}
	return nil
}
