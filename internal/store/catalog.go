package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BloodShotAK/pgblame/internal/catalog"
)

type CatalogChanges struct {
	Baseline bool
	Indexes  []catalog.Change[catalog.IndexKey, catalog.Index]
	Settings []catalog.Change[string, catalog.Setting]
	Columns  []catalog.Change[catalog.ColumnKey, catalog.ColumnStats]
}

type versionTable[K comparable, V any] struct {
	name    string
	keyCols []string
	valCols []string
	keyVals func(K) []any
	valVals func(V) []any
	scan    func(pgx.Rows) (K, V, error)
	changed func(old, cur V) bool
}

var indexTable = versionTable[catalog.IndexKey, catalog.Index]{
	name:    "index_versions",
	keyCols: []string{"schema_name", "table_name", "index_name"},
	valCols: []string{"definition", "is_valid", "is_ready", "is_unique", "is_primary"},
	keyVals: func(k catalog.IndexKey) []any { return []any{k.Schema, k.Table, k.Index} },
	valVals: func(v catalog.Index) []any { return []any{v.Definition, v.Valid, v.Ready, v.Unique, v.Primary} },
	scan: func(r pgx.Rows) (k catalog.IndexKey, v catalog.Index, err error) {
		err = r.Scan(&k.Schema, &k.Table, &k.Index, &v.Definition, &v.Valid, &v.Ready, &v.Unique, &v.Primary)
		return
	},
	changed: func(old, cur catalog.Index) bool { return old != cur },
}

var settingTable = versionTable[string, catalog.Setting]{
	name:    "setting_versions",
	keyCols: []string{"name"},
	valCols: []string{"value", "unit", "source"},
	keyVals: func(k string) []any { return []any{k} },
	valVals: func(v catalog.Setting) []any { return []any{v.Value, v.Unit, v.Source} },
	scan: func(r pgx.Rows) (k string, v catalog.Setting, err error) {
		err = r.Scan(&k, &v.Value, &v.Unit, &v.Source)
		return
	},
	// A source change alone (default -> config file) doesn't affect plans.
	changed: func(old, cur catalog.Setting) bool { return old.Value != cur.Value },
}

var columnTable = versionTable[catalog.ColumnKey, catalog.ColumnStats]{
	name:    "column_stat_versions",
	keyCols: []string{"schema_name", "table_name", "column_name", "inherited"},
	valCols: []string{"null_frac", "n_distinct", "correlation"},
	keyVals: func(k catalog.ColumnKey) []any { return []any{k.Schema, k.Table, k.Column, k.Inherited} },
	valVals: func(v catalog.ColumnStats) []any { return []any{v.NullFrac, v.NDistinct, v.Correlation} },
	scan: func(r pgx.Rows) (k catalog.ColumnKey, v catalog.ColumnStats, err error) {
		err = r.Scan(&k.Schema, &k.Table, &k.Column, &k.Inherited, &v.NullFrac, &v.NDistinct, &v.Correlation)
		return
	},
	changed: catalog.StatsShifted,
}

func (s *Store) ApplyCatalog(ctx context.Context, targetID int64, snap *catalog.Snapshot) (CatalogChanges, error) {
	var out CatalogChanges
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		var noIndexes, noSettings, noColumns bool
		if out.Indexes, noIndexes, err = applyVersions(ctx, tx, indexTable, targetID, snap.DBName, snap.Indexes, snap.TakenAt); err != nil {
			return err
		}
		if out.Settings, noSettings, err = applyVersions(ctx, tx, settingTable, targetID, snap.DBName, snap.Settings, snap.TakenAt); err != nil {
			return err
		}
		if out.Columns, noColumns, err = applyVersions(ctx, tx, columnTable, targetID, snap.DBName, snap.Columns, snap.TakenAt); err != nil {
			return err
		}
		// A new database may have no indexes or stats yet, so only an empty
		// record everywhere counts as a first visit.
		out.Baseline = noIndexes && noSettings && noColumns
		return writeTableStats(ctx, tx, targetID, snap)
	})
	return out, err
}

func applyVersions[K comparable, V any](ctx context.Context, tx pgx.Tx, t versionTable[K, V], targetID int64, dbname string, observed map[K]V, at time.Time) ([]catalog.Change[K, V], bool, error) {
	cols := slices.Concat(t.keyCols, t.valCols)
	rows, err := tx.Query(ctx, fmt.Sprintf(
		`SELECT %s FROM %s WHERE target_id = $1 AND dbname = $2 AND valid_to IS NULL`,
		strings.Join(cols, ", "), t.name), targetID, dbname)
	if err != nil {
		return nil, false, fmt.Errorf("loading %s: %w", t.name, err)
	}
	recorded := map[K]V{}
	for rows.Next() {
		k, v, err := t.scan(rows)
		if err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scanning %s: %w", t.name, err)
		}
		recorded[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("loading %s: %w", t.name, err)
	}

	changes := catalog.Diff(recorded, observed, t.changed)

	match := make([]string, len(t.keyCols))
	for i, c := range t.keyCols {
		match[i] = fmt.Sprintf("%s = $%d", c, i+5)
	}
	closeSQL := fmt.Sprintf(
		`UPDATE %s SET valid_to = $3, end_reason = $4 WHERE target_id = $1 AND dbname = $2 AND valid_to IS NULL AND %s`,
		t.name, strings.Join(match, " AND "))

	var inserts [][]any
	for _, c := range changes {
		if c.Kind != catalog.Added {
			args := append([]any{targetID, dbname, at, string(c.Kind)}, t.keyVals(c.Key)...)
			if _, err := tx.Exec(ctx, closeSQL, args...); err != nil {
				return nil, false, fmt.Errorf("closing %s version: %w", t.name, err)
			}
		}
		if c.Kind != catalog.Removed {
			inserts = append(inserts, slices.Concat([]any{targetID, dbname, at}, t.keyVals(c.Key), t.valVals(c.New)))
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{t.name}, slices.Concat([]string{"target_id", "dbname", "valid_from"}, cols), pgx.CopyFromRows(inserts)); err != nil {
		return nil, false, fmt.Errorf("writing %s: %w", t.name, err)
	}
	return changes, len(recorded) == 0, nil
}

func writeTableStats(ctx context.Context, tx pgx.Tx, targetID int64, snap *catalog.Snapshot) error {
	rows := make([][]any, 0, len(snap.Tables))
	for _, t := range snap.Tables {
		rows = append(rows, []any{
			targetID, snap.DBName, snap.TakenAt, t.Schema, t.Table,
			t.RelTuples, t.RelPages, t.LiveTuples, t.DeadTuples, t.LastAnalyze, t.LastAutoanalyze,
		})
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"table_stat_samples"}, []string{
		"target_id", "dbname", "taken_at", "schema_name", "table_name",
		"reltuples", "relpages", "n_live_tup", "n_dead_tup", "last_analyze", "last_autoanalyze",
	}, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("writing table statistics: %w", err)
	}
	return nil
}
