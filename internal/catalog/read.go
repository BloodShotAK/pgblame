package catalog

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func notSystem(col string) string {
	return fmt.Sprintf(`%[1]s NOT IN ('pg_catalog', 'information_schema') AND %[1]s NOT LIKE 'pg_toast%%'`, col)
}

func Read(ctx context.Context, q Querier) (*Snapshot, error) {
	s := &Snapshot{
		Indexes:  map[IndexKey]Index{},
		Settings: map[string]Setting{},
		Columns:  map[ColumnKey]ColumnStats{},
	}
	if err := q.QueryRow(ctx, `SELECT clock_timestamp(), current_database()`).Scan(&s.TakenAt, &s.DBName); err != nil {
		return nil, fmt.Errorf("reading database identity: %w", err)
	}
	if err := readIndexes(ctx, q, s); err != nil {
		return nil, err
	}
	if err := readSettings(ctx, q, s); err != nil {
		return nil, err
	}
	if err := readColumns(ctx, q, s); err != nil {
		return nil, err
	}
	if err := readTables(ctx, q, s); err != nil {
		return nil, err
	}
	return s, nil
}

func readIndexes(ctx context.Context, q Querier, s *Snapshot) error {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, t.relname, i.relname, pg_get_indexdef(ix.indexrelid),
		       ix.indisvalid, ix.indisready, ix.indisunique, ix.indisprimary
		FROM pg_index ix
		JOIN pg_class i     ON i.oid = ix.indexrelid
		JOIN pg_class t     ON t.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE `+notSystem("n.nspname"))
	if err != nil {
		return fmt.Errorf("reading indexes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k IndexKey
		var v Index
		if err := rows.Scan(&k.Schema, &k.Table, &k.Index, &v.Definition, &v.Valid, &v.Ready, &v.Unique, &v.Primary); err != nil {
			return fmt.Errorf("scanning index: %w", err)
		}
		s.Indexes[k] = v
	}
	return rows.Err()
}

// reset_val rather than setting, so a SET on our own session never looks like a server change.
func readSettings(ctx context.Context, q Querier, s *Snapshot) error {
	rows, err := q.Query(ctx, `
		SELECT name, coalesce(reset_val, ''), coalesce(unit, ''), source
		FROM pg_settings
		WHERE category LIKE 'Query Tuning%'
		   OR category LIKE 'Resource Usage%'
		   OR category ILIKE '%autovacuum%'
		   OR category ILIKE '%automatic vacuuming%'`)
	if err != nil {
		return fmt.Errorf("reading settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var v Setting
		if err := rows.Scan(&name, &v.Value, &v.Unit, &v.Source); err != nil {
			return fmt.Errorf("scanning setting: %w", err)
		}
		s.Settings[name] = v
	}
	return rows.Err()
}

// pg_stats hides columns we can't SELECT; the helper in deploy/target/helper.sql gets around that.
func readColumns(ctx context.Context, q Querier, s *Snapshot) error {
	if err := q.QueryRow(ctx, `SELECT to_regprocedure('pgblame.column_stats()') IS NOT NULL`).Scan(&s.ColumnsComplete); err != nil {
		return fmt.Errorf("checking for pgblame.column_stats(): %w", err)
	}
	src := `pgblame.column_stats()`
	if !s.ColumnsComplete {
		src = `(SELECT schemaname, tablename, attname, inherited,
		               null_frac::float8 AS null_frac, n_distinct::float8 AS n_distinct,
		               correlation::float8 AS correlation
		        FROM pg_stats WHERE ` + notSystem("schemaname") + `) s`
	}
	rows, err := q.Query(ctx, `SELECT schemaname, tablename, attname, inherited, null_frac, n_distinct, correlation FROM `+src)
	if err != nil {
		return fmt.Errorf("reading column statistics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k ColumnKey
		var v ColumnStats
		if err := rows.Scan(&k.Schema, &k.Table, &k.Column, &k.Inherited, &v.NullFrac, &v.NDistinct, &v.Correlation); err != nil {
			return fmt.Errorf("scanning column statistics: %w", err)
		}
		s.Columns[k] = v
	}
	return rows.Err()
}

func readTables(ctx context.Context, q Querier, s *Snapshot) error {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname, c.reltuples::bigint, c.relpages,
		       coalesce(st.n_live_tup, 0), coalesce(st.n_dead_tup, 0),
		       st.last_analyze, st.last_autoanalyze
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_stat_user_tables st ON st.relid = c.oid
		WHERE c.relkind IN ('r', 'p', 'm') AND `+notSystem("n.nspname"))
	if err != nil {
		return fmt.Errorf("reading table statistics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t TableStats
		if err := rows.Scan(&t.Schema, &t.Table, &t.RelTuples, &t.RelPages, &t.LiveTuples, &t.DeadTuples, &t.LastAnalyze, &t.LastAutoanalyze); err != nil {
			return fmt.Errorf("scanning table statistics: %w", err)
		}
		s.Tables = append(s.Tables, t)
	}
	return rows.Err()
}
