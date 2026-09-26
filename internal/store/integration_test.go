package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/BloodShotAK/pgblame/internal/catalog"
	"github.com/BloodShotAK/pgblame/internal/pgss"
	"github.com/BloodShotAK/pgblame/internal/store"
	"github.com/BloodShotAK/pgblame/internal/testpg"
)

func TestIntegrationStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := testpg.NewDatabase(t)
	s := store.New(db)
	for range 2 {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate should be repeatable: %v", err)
		}
	}
	target, err := s.EnsureTarget(ctx, "it")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.EnsureTarget(ctx, "it"); again != target {
		t.Fatalf("EnsureTarget not stable: %d then %d", target, again)
	}

	t.Run("statements", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		key := pgss.Key{UserID: 10, DBID: 16384, QueryID: -42, TopLevel: true}
		text := "SELECT * FROM orders WHERE id = $1"
		entry := pgss.Entry{Key: key, DBName: "app", UserName: "app"}
		snap := &pgss.Snapshot{TakenAt: now, ServerVersionNum: 170000, Entries: map[pgss.Key]pgss.Entry{key: entry}}

		if err := s.WriteSnapshot(ctx, target, snap, nil, nil); err != nil {
			t.Fatalf("baseline: %v", err)
		}
		ids, err := s.AddQueries(ctx, target, []store.NewQuery{{Entry: entry, Text: &text}}, now)
		if err != nil {
			t.Fatal(err)
		}
		iv := &pgss.Interval{Start: now.Add(-time.Minute), End: now, Samples: []pgss.Sample{
			{Key: key, Delta: pgss.Counters{Calls: 40, ExecMS: 100, Rows: 40}},
		}}
		if err := s.WriteSnapshot(ctx, target, snap, iv, ids); err != nil {
			t.Fatalf("interval: %v", err)
		}

		known, _, err := s.QueryIDs(ctx, target)
		if err != nil || known[key] != ids[key] {
			t.Fatalf("QueryIDs = %v (err %v), want %v", known, err, ids)
		}
		top, err := s.Top(ctx, now.Add(-time.Hour), 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(top) != 1 || top[0].Calls != 40 || top[0].MeanMS != 2.5 || top[0].Text != text {
			t.Errorf("unexpected top: %+v", top)
		}
	})

	t.Run("catalog versions", func(t *testing.T) {
		t0 := time.Now().UTC().Truncate(time.Microsecond)
		idx := catalog.IndexKey{Schema: "public", Table: "orders", Index: "orders_customer_idx"}
		snap := func(at time.Time, indexes map[catalog.IndexKey]catalog.Index, rpc string) *catalog.Snapshot {
			return &catalog.Snapshot{
				TakenAt: at, DBName: "app", Indexes: indexes,
				Settings: map[string]catalog.Setting{"random_page_cost": {Value: rpc, Source: "default"}},
				Columns:  map[catalog.ColumnKey]catalog.ColumnStats{},
				Tables:   []catalog.TableStats{{TableKey: catalog.TableKey{Schema: "public", Table: "orders"}, TableSize: catalog.TableSize{RelTuples: 5000}}},
			}
		}

		ch, err := s.ApplyCatalog(ctx, target, snap(t0, map[catalog.IndexKey]catalog.Index{idx: {Definition: "CREATE INDEX ...", Valid: true, Ready: true}}, "4"))
		if err != nil {
			t.Fatal(err)
		}
		if !ch.Baseline {
			t.Error("first catalog write should be a baseline")
		}

		t1 := t0.Add(5 * time.Minute)
		ch, err = s.ApplyCatalog(ctx, target, snap(t1, map[catalog.IndexKey]catalog.Index{}, "1.1"))
		if err != nil {
			t.Fatal(err)
		}
		if ch.Baseline || len(ch.Indexes) != 1 || ch.Indexes[0].Kind != catalog.Removed {
			t.Errorf("want one removed index, got %+v", ch.Indexes)
		}
		if len(ch.Settings) != 1 || ch.Settings[0].Old.Value != "4" || ch.Settings[0].New.Value != "1.1" {
			t.Errorf("want random_page_cost 4 -> 1.1, got %+v", ch.Settings)
		}

		var validTo time.Time
		var reason string
		err = db.QueryRow(ctx, `SELECT valid_to, end_reason FROM index_versions WHERE index_name = $1`, idx.Index).Scan(&validTo, &reason)
		if err != nil || !validTo.Equal(t1) || reason != "removed" {
			t.Errorf("index version not closed at %v: valid_to=%v reason=%q err=%v", t1, validTo, reason, err)
		}
		var open int
		db.QueryRow(ctx, `SELECT count(*) FROM setting_versions WHERE name = 'random_page_cost' AND valid_to IS NULL`).Scan(&open)
		if open != 1 {
			t.Errorf("want exactly one open random_page_cost version, got %d", open)
		}

		ch, err = s.ApplyCatalog(ctx, target, snap(t1.Add(5*time.Minute), map[catalog.IndexKey]catalog.Index{}, "1.1"))
		if err != nil || len(ch.Indexes)+len(ch.Settings)+len(ch.Columns) != 0 {
			t.Errorf("unchanged catalog produced changes: %+v (err %v)", ch, err)
		}
	})

	t.Run("first index on a new database", func(t *testing.T) {
		t0 := time.Now().UTC().Truncate(time.Microsecond)
		empty := &catalog.Snapshot{TakenAt: t0, DBName: "fresh",
			Settings: map[string]catalog.Setting{"work_mem": {Value: "4096"}}}
		if ch, err := s.ApplyCatalog(ctx, target, empty); err != nil || !ch.Baseline {
			t.Fatalf("first visit should be a baseline: %+v (err %v)", ch, err)
		}
		withIndex := *empty
		withIndex.TakenAt = t0.Add(time.Minute)
		withIndex.Indexes = map[catalog.IndexKey]catalog.Index{{Schema: "public", Table: "t", Index: "t_pkey"}: {Valid: true}}
		ch, err := s.ApplyCatalog(ctx, target, &withIndex)
		if err != nil || ch.Baseline || len(ch.Indexes) != 1 || ch.Indexes[0].Kind != catalog.Added {
			t.Errorf("want a non-baseline added index, got %+v (err %v)", ch, err)
		}
	})
}
