package catalog_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/BloodShotAK/pgculprit/internal/catalog"
	"github.com/BloodShotAK/pgculprit/internal/testpg"
)

func TestIntegrationCatalogExplainsPlanChanges(t *testing.T) {
	ctx := context.Background()
	db := testpg.NewDatabase(t)
	exec := func(sql string) {
		t.Helper()
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	read := func() *catalog.Snapshot {
		t.Helper()
		s, err := catalog.Read(ctx, db, nil)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		return s
	}

	exec(`CREATE TABLE orders (id int PRIMARY KEY, customer_id int, status text)`)
	exec(`INSERT INTO orders SELECT i, i % 10, 'open' FROM generate_series(1, 5000) i`)
	exec(`CREATE INDEX orders_customer_idx ON orders (customer_id)`)
	// Fails on the duplicate statuses and leaves an invalid index behind.
	if _, err := db.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY orders_status_uniq ON orders (status)`); err == nil {
		t.Fatal("expected the unique index build to fail")
	}
	exec(`ANALYZE orders`)

	before := read()
	idx := catalog.IndexKey{Schema: "public", Table: "orders", Index: "orders_customer_idx"}
	if !before.Indexes[idx].Valid {
		t.Fatalf("want %s valid, got %+v", idx.Index, before.Indexes[idx])
	}
	invalid := catalog.IndexKey{Schema: "public", Table: "orders", Index: "orders_status_uniq"}
	if v, ok := before.Indexes[invalid]; !ok || v.Valid {
		t.Fatalf("want %s present but invalid, got %+v (present=%v)", invalid.Index, v, ok)
	}
	col := catalog.ColumnKey{Schema: "public", Table: "orders", Column: "customer_id"}
	if got := before.Columns[col].NDistinct; got != 10 {
		t.Fatalf("customer_id n_distinct = %v, want 10", got)
	}
	if len(before.Settings) == 0 || before.Settings["random_page_cost"].Value == "" {
		t.Fatal("planner settings missing from snapshot")
	}

	exec(`DROP INDEX orders_customer_idx`)
	exec(`UPDATE orders SET customer_id = id`)
	exec(`ANALYZE orders`)
	after := read()

	idxChanges := catalog.Diff(before.Indexes, after.Indexes, func(a, b catalog.Index) bool { return a != b })
	if len(idxChanges) != 1 || idxChanges[0].Kind != catalog.Removed || idxChanges[0].Key != idx {
		t.Errorf("want only %s removed, got %+v", idx.Index, idxChanges)
	}

	colChanges := catalog.Diff(before.Columns, after.Columns, catalog.StatsShifted)
	found := false
	for _, c := range colChanges {
		if c.Key == col && c.Kind == catalog.Modified {
			found = true
			if c.New.NDistinct != -1 {
				t.Errorf("customer_id is now unique; want n_distinct -1, got %v", c.New.NDistinct)
			}
		}
	}
	if !found {
		t.Errorf("customer_id distribution shift not detected: %+v", colChanges)
	}

	// Before PG15, last_analyze reaches pg_stat_user_tables through the
	// asynchronous stats collector, so wait until it shows.
	analyze := func(table string) {
		t.Helper()
		var before *time.Time
		db.QueryRow(ctx, `SELECT last_analyze FROM pg_stat_user_tables WHERE relname = $1`, table).Scan(&before)
		exec(`ANALYZE ` + table)
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			var after *time.Time
			db.QueryRow(ctx, `SELECT last_analyze FROM pg_stat_user_tables WHERE relname = $1`, table).Scan(&after)
			if after != nil && (before == nil || after.After(*before)) {
				return
			}
		}
		t.Fatalf("ANALYZE %s never showed up in pg_stat_user_tables", table)
	}

	t.Run("only analyzed tables are re-read", func(t *testing.T) {
		helper, err := os.ReadFile("../../deploy/target/helper.sql")
		if err != nil {
			t.Fatal(err)
		}
		exec(`DO $$ BEGIN
		        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgculprit') THEN CREATE ROLE pgculprit; END IF;
		      END $$`)
		exec(string(helper))
		exec(`CREATE TABLE untouched (a int)`)
		exec(`INSERT INTO untouched SELECT generate_series(1, 100)`)
		analyze("untouched")

		full := read()
		if !full.ColumnsComplete {
			t.Fatal("helper installed but not used")
		}
		analyzed := map[catalog.TableKey]time.Time{}
		for _, tb := range full.Tables {
			analyzed[tb.TableKey] = tb.AnalyzedAt
		}

		idle, err := catalog.Read(ctx, db, analyzed)
		if err != nil {
			t.Fatal(err)
		}
		if len(idle.ColumnTables) != 0 || len(idle.Columns) != 0 {
			t.Errorf("nothing was analyzed, yet read %d columns from %v", len(idle.Columns), idle.ColumnTables)
		}

		analyze("orders")
		orders := catalog.TableKey{Schema: "public", Table: "orders"}
		inc, err := catalog.Read(ctx, db, analyzed)
		if err != nil {
			t.Fatal(err)
		}
		if len(inc.ColumnTables) != 1 || !inc.ColumnTables[orders] || len(inc.Columns) == 0 {
			t.Fatalf("want columns of orders only, got tables %v and %d columns", inc.ColumnTables, len(inc.Columns))
		}
		for k := range inc.Columns {
			if k.Table != "orders" {
				t.Errorf("read column of %s", k.Table)
			}
		}
	})
}
