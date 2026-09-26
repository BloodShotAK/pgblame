package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/BloodShotAK/pgculprit/internal/pgss"
	"github.com/BloodShotAK/pgculprit/internal/store"
	"github.com/BloodShotAK/pgculprit/internal/testpg"
)

func TestIntegrationMaintainBoundsTheStore(t *testing.T) {
	ctx := context.Background()
	db := testpg.NewDatabase(t)
	s := store.New(db)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	target, _ := s.EnsureTarget(ctx, "it")
	key := pgss.Key{UserID: 1, DBID: 1, QueryID: 9, TopLevel: true}
	ids, _ := s.AddQueries(ctx, target, []store.NewQuery{{Entry: pgss.Entry{Key: key}}}, time.Now())
	qid := ids[key]

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	insert := func(end time.Time, partial bool) {
		exec(`INSERT INTO query_samples (query_id, interval_start, interval_end, partial, calls, plans, exec_ms, plan_ms,
			    rows, shared_blks_hit, shared_blks_read, shared_blks_dirtied, shared_blks_written,
			    temp_blks_read, temp_blks_written, wal_bytes)
			VALUES ($1, $2, $3, $4, 10, 0, 20, 0, 10, 40, 0, 0, 0, 0, 0, 0)`, qid, end.Add(-time.Minute), end, partial)
	}

	now := time.Now().UTC()
	hour := now.Truncate(time.Hour)
	for m := 1; m <= 60; m++ {
		insert(hour.Add(-time.Hour).Add(time.Duration(m)*time.Minute), m == 30)
	}
	old := now.Truncate(24*time.Hour).AddDate(0, 0, -10)
	exec(`CREATE TABLE query_samples_` + old.Format("20060102") + ` PARTITION OF query_samples
	      FOR VALUES FROM ('` + old.Format(time.RFC3339) + `') TO ('` + old.AddDate(0, 0, 1).Format(time.RFC3339) + `')`)
	insert(old.Add(12*time.Hour), false)
	exec(`INSERT INTO query_samples_hourly VALUES ($1, $2, 1, 1, 1, 1, 1)`, qid, now.AddDate(0, 0, -40).Truncate(time.Hour))

	res, err := s.Maintain(ctx, now, store.DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DroppedDays) != 1 || res.DroppedDays[0] != old.Format(time.DateOnly) {
		t.Errorf("want only %s dropped, got %v", old.Format(time.DateOnly), res.DroppedDays)
	}
	if res.DeletedHourly != 1 {
		t.Errorf("want the 40-day-old rollup deleted, got %d", res.DeletedHourly)
	}

	var calls int64
	var hours int
	db.QueryRow(ctx, `SELECT sum(calls), count(*) FROM query_samples_hourly WHERE hour_start = $1`, hour.Add(-time.Hour)).Scan(&calls, &hours)
	// The previous hour's last interval ends on the hour, so it starts inside it
	// and belongs to it; the partial one is left out.
	if hours != 1 || calls != 590 {
		t.Errorf("want one rollup row with 59 intervals x 10 calls, got %d rows with %d calls", hours, calls)
	}

	for d := -1; d <= 2; d++ {
		name := "query_samples_" + now.Truncate(24*time.Hour).AddDate(0, 0, d).Format("20060102")
		var exists bool
		db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists)
		if !exists {
			t.Errorf("partition %s missing", name)
		}
	}

	if again, err := s.Maintain(ctx, now, store.DefaultRetention()); err != nil || len(again.DroppedDays) != 0 {
		t.Errorf("second run should be a no-op for drops: %+v (err %v)", again, err)
	}
}
