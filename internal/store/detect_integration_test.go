package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/BloodShotAK/pgblame/internal/detect"
	"github.com/BloodShotAK/pgblame/internal/pgss"
	"github.com/BloodShotAK/pgblame/internal/store"
	"github.com/BloodShotAK/pgblame/internal/testpg"
)

func TestIntegrationRegressionLifecycle(t *testing.T) {
	ctx := context.Background()
	db := testpg.NewDatabase(t)
	s := store.New(db)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	target, _ := s.EnsureTarget(ctx, "it")
	now := time.Now().UTC().Truncate(time.Minute)
	key := pgss.Key{UserID: 1, DBID: 1, QueryID: 7, TopLevel: true}
	ids, err := s.AddQueries(ctx, target, []store.NewQuery{{Entry: pgss.Entry{Key: key}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	qid := ids[key]

	sample := func(end time.Time, ms float64, evictionSuspect bool) {
		t.Helper()
		_, err := db.Exec(ctx, `
			INSERT INTO query_samples (query_id, interval_start, interval_end, partial, calls, plans, exec_ms, plan_ms,
			    rows, shared_blks_hit, shared_blks_read, shared_blks_dirtied, shared_blks_written,
			    temp_blks_read, temp_blks_written, wal_bytes)
			VALUES ($1, $2, $3, false, 100, 0, $4, 0, 100, 400, 0, 0, 0, 0, 0, 0)`,
			qid, end.Add(-time.Minute), end, ms*100)
		if err == nil {
			_, err = db.Exec(ctx, `
				INSERT INTO pgss_snapshots (target_id, taken_at, server_version_num, dealloc, entries, eviction_suspect)
				VALUES ($1, $2, 170000, 0, 1, $3)`, target, end, evictionSuspect)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for m := 60; m > 15; m-- {
		sample(now.Add(-time.Duration(m)*time.Minute), 2, m == 30)
	}
	for m := 15; m > 0; m-- {
		sample(now.Add(-time.Duration(m-1)*time.Minute), 20, false)
	}

	cfg := detect.DefaultConfig()
	series, err := s.Series(ctx, target, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(series[qid]); got != 59 {
		t.Fatalf("want 59 points (60 minus the eviction-suspect one), got %d", got)
	}

	found, healthy := detect.Scan(series, now, cfg)
	if len(found) != 1 || found[0].Kind != detect.SlowerSameWork {
		t.Fatalf("want one finding, got %+v (healthy %v)", found, healthy)
	}
	opened, _, err := s.ApplyFindings(ctx, now, found, healthy)
	if err != nil || len(opened) != 1 {
		t.Fatalf("want the regression opened, got %v (err %v)", opened, err)
	}
	if opened, _, _ := s.ApplyFindings(ctx, now.Add(time.Minute), found, nil); len(opened) != 0 {
		t.Errorf("a second finding should refresh the open regression, not open another")
	}

	// Half an hour on, the slow intervals sit in the trailing baseline, but
	// belong to the open regression and must not count as normal.
	later := now.Add(30 * time.Minute)
	for m := 30; m > 0; m-- {
		sample(later.Add(-time.Duration(m-1)*time.Minute), 20, false)
	}
	series, _ = s.Series(ctx, target, cfg, later)
	found, _ = detect.Scan(series, later, cfg)
	if len(found) != 1 || found[0].BaselineMS != 2 {
		t.Fatalf("baseline should still be the pre-regression 2ms, got %+v", found)
	}

	_, closed, err := s.ApplyFindings(ctx, later, nil, []int64{qid})
	if err != nil || len(closed) != 1 {
		t.Fatalf("want the regression closed, got %v (err %v)", closed, err)
	}
	var open int
	db.QueryRow(ctx, `SELECT count(*) FROM regressions WHERE closed_at IS NULL`).Scan(&open)
	if open != 0 {
		t.Errorf("%d regressions still open", open)
	}
}
