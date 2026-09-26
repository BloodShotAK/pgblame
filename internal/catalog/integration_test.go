package catalog_test

import (
	"context"
	"testing"

	"github.com/BloodShotAK/pgblame/internal/catalog"
	"github.com/BloodShotAK/pgblame/internal/testpg"
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
		s, err := catalog.Read(ctx, db)
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
}
