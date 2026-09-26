package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BloodShotAK/pgculprit/internal/catalog"
)

type CatalogChanges struct {
	Baseline bool
	Indexes  []catalog.Change[catalog.IndexKey, catalog.Index]
	Settings []catalog.Change[string, catalog.Setting]
	Tables   []catalog.Change[catalog.TableKey, catalog.TableSize]
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

var tableTable = versionTable[catalog.TableKey, catalog.TableSize]{
	name:    "table_stat_versions",
	keyCols: []string{"schema_name", "table_name"},
	valCols: []string{"reltuples", "relpages"},
	keyVals: func(k catalog.TableKey) []any { return []any{k.Schema, k.Table} },
	valVals: func(v catalog.TableSize) []any { return []any{v.RelTuples, v.RelPages} },
	scan: func(r pgx.Rows) (k catalog.TableKey, v catalog.TableSize, err error) {
		err = r.Scan(&k.Schema, &k.Table, &v.RelTuples, &v.RelPages)
		return
	},
	changed: catalog.SizeShifted,
}

func (s *Store) ApplyCatalog(ctx context.Context, targetID int64, snap *catalog.Snapshot) (CatalogChanges, error) {
	var out CatalogChanges
	tables := make(map[catalog.TableKey]catalog.TableSize, len(snap.Tables))
	for _, t := range snap.Tables {
		tables[t.TableKey] = t.TableSize
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		var noIndexes, noSettings, noTables bool
		if out.Indexes, noIndexes, err = applyVersions(ctx, tx, indexTable, targetID, snap.DBName, snap.Indexes, snap.TakenAt, nil); err != nil {
			return err
		}
		if out.Settings, noSettings, err = applyVersions(ctx, tx, settingTable, targetID, snap.DBName, snap.Settings, snap.TakenAt, nil); err != nil {
			return err
		}
		if out.Tables, noTables, err = applyVersions(ctx, tx, tableTable, targetID, snap.DBName, tables, snap.TakenAt, nil); err != nil {
			return err
		}
		// A new database may have no indexes yet, so only an empty record
		// everywhere counts as a first visit.
		out.Baseline = noIndexes && noSettings && noTables
		// Columns of tables that weren't re-read stay as recorded; dropped
		// tables are added to the scope so their columns get closed.
		var scope []catalog.TableKey
		if snap.ColumnTables != nil {
			scope = slices.Collect(maps.Keys(snap.ColumnTables))
			for _, c := range out.Tables {
				if c.Kind == catalog.Removed {
					scope = append(scope, c.Key)
				}
			}
			if len(scope) == 0 {
				return nil
			}
		}
		out.Columns, _, err = applyVersions(ctx, tx, columnTable, targetID, snap.DBName, snap.Columns, snap.TakenAt, scope)
		return err
	})
	return out, err
}

// applyVersions diffs observed against the open versions and records the
// changes. A non-nil scope limits both sides to those tables.
func applyVersions[K comparable, V any](ctx context.Context, tx pgx.Tx, t versionTable[K, V], targetID int64, dbname string, observed map[K]V, at time.Time, scope []catalog.TableKey) ([]catalog.Change[K, V], bool, error) {
	cols := slices.Concat(t.keyCols, t.valCols)
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE target_id = $1 AND dbname = $2 AND valid_to IS NULL`, strings.Join(cols, ", "), t.name)
	args := []any{targetID, dbname}
	if scope != nil {
		var schemas, names []string
		for _, k := range scope {
			schemas, names = append(schemas, k.Schema), append(names, k.Table)
		}
		query += ` AND (schema_name, table_name) IN (SELECT * FROM unnest($3::text[], $4::text[]))`
		args = append(args, schemas, names)
	}
	rows, err := tx.Query(ctx, query, args...)
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
