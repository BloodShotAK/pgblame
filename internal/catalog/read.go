package catalog

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

func notSystem(col string) string {
	return fmt.Sprintf(`%[1]s NOT IN ('pg_catalog', 'information_schema') AND %[1]s NOT LIKE 'pg_toast%%'`, col)
}

// Read snapshots the catalog. Column statistics only change on ANALYZE, so
// with analyzed from the previous snapshot they are re-read only for tables
// analyzed since; a nil analyzed reads them all.
func Read(ctx context.Context, q Querier, analyzed map[TableKey]time.Time) (*Snapshot, error) {
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
	if err := readTables(ctx, q, s); err != nil {
		return nil, err
	}
	if analyzed != nil {
		s.ColumnTables = map[TableKey]bool{}
		for _, t := range s.Tables {
			if !t.AnalyzedAt.Equal(analyzed[t.TableKey]) {
				s.ColumnTables[t.TableKey] = true
			}
		}
	}
	if err := readColumns(ctx, q, s); err != nil {
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
// columnChunk bounds how many tables one statement covers, so no single
// read runs long on a database with many tables.
const columnChunk = 250

func readColumns(ctx context.Context, q Querier, s *Snapshot) error {
	if err := q.QueryRow(ctx, `SELECT to_regprocedure('pgculprit.column_stats(name[],name[])') IS NOT NULL`).Scan(&s.ColumnsComplete); err != nil {
		return fmt.Errorf("checking for pgculprit.column_stats(): %w", err)
	}
	var tables []TableKey
	for _, t := range s.Tables {
		if s.ColumnTables == nil || s.ColumnTables[t.TableKey] {
			tables = append(tables, t.TableKey)
		}
	}
	for chunk := range slices.Chunk(tables, columnChunk) {
		var err error
		if s.ColumnsComplete {
			err = readColumnsHelper(ctx, q, s, chunk)
		} else {
			err = readColumnsView(ctx, q, s, chunk)
		}
		if err != nil {
			return fmt.Errorf("reading column statistics: %w", err)
		}
	}
	return nil
}

func readColumnsHelper(ctx context.Context, q Querier, s *Snapshot, tables []TableKey) error {
	schemas, names := make([]string, len(tables)), make([]string, len(tables))
	for i, t := range tables {
		schemas[i], names[i] = t.Schema, t.Table
	}
	rows, err := q.Query(ctx, `SELECT * FROM pgculprit.column_stats($1::name[], $2::name[])`, schemas, names)
	if err != nil {
		return err
	}
	return scanColumns(rows, s)
}

// Without the helper, pg_stats is queried per table in one pipelined batch;
// constant filters are the only ones it lets through to the catalog indexes.
func readColumnsView(ctx context.Context, q Querier, s *Snapshot, tables []TableKey) error {
	b := &pgx.Batch{}
	for _, t := range tables {
		b.Queue(`SELECT schemaname, tablename, attname, inherited, null_frac::float8, n_distinct::float8, correlation::float8
		         FROM pg_stats WHERE schemaname = $1 AND tablename = $2`, t.Schema, t.Table)
	}
	br := q.SendBatch(ctx, b)
	defer br.Close()
	for range tables {
		rows, err := br.Query()
		if err != nil {
			return err
		}
		if err := scanColumns(rows, s); err != nil {
			return err
		}
	}
	return br.Close()
}

func scanColumns(rows pgx.Rows, s *Snapshot) error {
	defer rows.Close()
	for rows.Next() {
		var k ColumnKey
		var v ColumnStats
		if err := rows.Scan(&k.Schema, &k.Table, &k.Column, &k.Inherited, &v.NullFrac, &v.NDistinct, &v.Correlation); err != nil {
			return err
		}
		s.Columns[k] = v
	}
	return rows.Err()
}

func readTables(ctx context.Context, q Querier, s *Snapshot) error {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname, c.reltuples::bigint, c.relpages,
		       greatest(st.last_analyze, st.last_autoanalyze)
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
		var analyzedAt *time.Time
		if err := rows.Scan(&t.Schema, &t.Table, &t.RelTuples, &t.RelPages, &analyzedAt); err != nil {
			return fmt.Errorf("scanning table statistics: %w", err)
		}
		if analyzedAt != nil {
			t.AnalyzedAt = *analyzedAt
		}
		s.Tables = append(s.Tables, t)
	}
	return rows.Err()
}
