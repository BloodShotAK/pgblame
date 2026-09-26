// Command overhead measures what pgculprit's reads cost on a large database.
// It creates (and drops) its own database, so point it at a disposable server.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BloodShotAK/pgculprit/internal/catalog"
	"github.com/BloodShotAK/pgculprit/internal/pgss"
)

const dbName = "pgculprit_overhead"

func main() {
	dsn := flag.String("target-dsn", os.Getenv("PGCULPRIT_TARGET_DSN"), "superuser DSN for a disposable server")
	tables := flag.Int("tables", 2000, "tables to create, each with 10 columns and 2 indexes")
	yes := flag.Bool("yes", false, "confirm the target server is disposable")
	reuse := flag.Bool("reuse", false, "keep the tables from a previous run")
	flag.Parse()
	if !*yes {
		fmt.Fprintf(os.Stderr, "overhead drops and recreates database %s on the target; rerun with -yes\n", dbName)
		os.Exit(2)
	}
	if err := run(context.Background(), *dsn, *tables, *reuse); err != nil {
		fmt.Fprintln(os.Stderr, "overhead:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn string, n int, reuse bool) error {
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer admin.Close()
	if !reuse {
		for _, stmt := range []string{`DROP DATABASE IF EXISTS ` + dbName + ` WITH (FORCE)`, `CREATE DATABASE ` + dbName} {
			if _, err := admin.Exec(ctx, stmt); err != nil {
				return err
			}
		}
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = dbName
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	if !reuse {
		if err := createTables(ctx, db, n); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "running %d distinct queries\n", 2*n)
	batch := &pgx.Batch{}
	for i := 1; i <= n; i++ {
		batch.Queue(fmt.Sprintf(`SELECT * FROM t%d WHERE a = $1`, i), 1)
		batch.Queue(fmt.Sprintf(`SELECT count(*) FROM t%d WHERE b > $1`, i), 3)
	}
	if err := db.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}

	conn, err := db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	c := conn.Conn()

	type row struct {
		op, size string
		took     time.Duration
	}
	var rows []row
	median := func(f func() (string, error)) (string, time.Duration, error) {
		var ts []time.Duration
		var size string
		for range 5 {
			t0 := time.Now()
			s, err := f()
			if err != nil {
				return "", 0, err
			}
			ts = append(ts, time.Since(t0))
			size = s
		}
		slices.Sort(ts)
		return size, ts[2], nil
	}

	var snap *pgss.Snapshot
	size, took, err := median(func() (string, error) {
		snap, err = pgss.Read(ctx, c, pgss.ReadOptions{})
		return fmt.Sprintf("%d entries", len(snap.Entries)), err
	})
	if err != nil {
		return err
	}
	rows = append(rows, row{"pg_stat_statements snapshot (every poll)", size, took})

	keys := make([]pgss.Key, 0, len(snap.Entries))
	for k := range snap.Entries {
		keys = append(keys, k)
	}
	size, took, err = median(func() (string, error) {
		texts, err := pgss.Texts(ctx, c, keys)
		return fmt.Sprintf("%d texts", len(texts)), err
	})
	if err != nil {
		return err
	}
	rows = append(rows, row{"query texts (at most every 5 min)", size, took})

	var full *catalog.Snapshot
	size, took, err = median(func() (string, error) {
		full, err = catalog.Read(ctx, c, nil)
		return fmt.Sprintf("%d tables, %d indexes, %d columns", len(full.Tables), len(full.Indexes), len(full.Columns)), err
	})
	if err != nil {
		return err
	}
	rows = append(rows, row{"catalog, full (startup and daily)", size, took})

	analyzed := map[catalog.TableKey]time.Time{}
	for _, t := range full.Tables {
		analyzed[t.TableKey] = t.AnalyzedAt
	}
	size, took, err = median(func() (string, error) {
		s, err := catalog.Read(ctx, c, analyzed)
		return fmt.Sprintf("%d columns re-read", len(s.Columns)), err
	})
	if err != nil {
		return err
	}
	rows = append(rows, row{"catalog, nothing analyzed (every 5 min)", size, took})

	if _, err := c.Exec(ctx, `ANALYZE t1, t2, t3, t4, t5, t6, t7, t8, t9, t10`); err != nil {
		return err
	}
	size, took, err = median(func() (string, error) {
		s, err := catalog.Read(ctx, c, analyzed)
		return fmt.Sprintf("%d columns re-read", len(s.Columns)), err
	})
	if err != nil {
		return err
	}
	rows = append(rows, row{"catalog, 10 tables analyzed (every 5 min)", size, took})

	helper, err := os.ReadFile("deploy/target/helper.sql")
	if err != nil {
		return err
	}
	if _, err := c.Exec(ctx, string(helper)); err != nil {
		return fmt.Errorf("installing helper: %w", err)
	}
	size, took, err = median(func() (string, error) {
		s, err := catalog.Read(ctx, c, nil)
		return fmt.Sprintf("%d columns", len(s.Columns)), err
	})
	if err != nil {
		return err
	}
	rows = append(rows, row{"catalog, full, via helper", size, took})
	if _, err := c.Exec(ctx, `ANALYZE t11, t12, t13, t14, t15, t16, t17, t18, t19, t20`); err != nil {
		return err
	}
	size, took, err = median(func() (string, error) {
		s, err := catalog.Read(ctx, c, analyzed)
		return fmt.Sprintf("%d columns re-read", len(s.Columns)), err
	})
	if err != nil {
		return err
	}
	rows = append(rows, row{"catalog, 20 tables analyzed, via helper", size, took})

	fmt.Println("| read | size | median of 5 |")
	fmt.Println("|---|---|---|")
	for _, r := range rows {
		fmt.Printf("| %s | %s | %s |\n", r.op, r.size, r.took.Round(100*time.Microsecond))
	}
	return nil
}

func createTables(ctx context.Context, db *pgxpool.Pool, n int) error {
	fmt.Fprintf(os.Stderr, "creating %d tables\n", n)
	if _, err := db.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		return err
	}
	// In batches, since one transaction can't hold locks on thousands of new tables.
	for from := 1; from <= n; from += 100 {
		_, err := db.Exec(ctx, fmt.Sprintf(`
			DO $$
			BEGIN
				FOR i IN %d..%d LOOP
					EXECUTE format('CREATE TABLE t%%s (id int PRIMARY KEY, a int, b int, c text, d timestamptz,
					                e numeric, f bool, g int, h text, i int)', i);
					EXECUTE format('INSERT INTO t%%s SELECT g, g %%%% 50, g %%%% 7, md5(g::text), now(), g, g %%%% 2 = 0, g, ''x'', g
					                FROM generate_series(1, 200) g', i);
					EXECUTE format('CREATE INDEX ON t%%s (a)', i);
				END LOOP;
			END $$`, from, min(from+99, n)))
		if err != nil {
			return err
		}
	}
	_, err := db.Exec(ctx, `ANALYZE`)
	return err
}
