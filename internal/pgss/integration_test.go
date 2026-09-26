package pgss_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BloodShotAK/pgculprit/internal/pgss"
	"github.com/BloodShotAK/pgculprit/internal/testpg"
)

func TestIntegrationDeltasMatchWorkload(t *testing.T) {
	ctx := context.Background()
	pool := testpg.Pool(t)

	// A fresh table gives the probe a unique queryid (the table OID is part of it).
	table := testpg.Unique("pgculprit_probe")
	mustExec(t, pool, "CREATE TABLE "+table+" (x int)")
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	probe := "SELECT count(*) FROM " + table + " WHERE x = $1"
	run := func(n int) {
		for i := range n {
			mustExec(t, pool, probe, i)
		}
	}
	run(1)

	var key pgss.Key
	err := pool.QueryRow(ctx, `
		SELECT userid, dbid, queryid, toplevel FROM pg_stat_statements
		WHERE query LIKE 'SELECT count(*) FROM ' || $1::text || '%'`, table).
		Scan(&key.UserID, &key.DBID, &key.QueryID, &key.TopLevel)
	if err != nil {
		t.Fatalf("finding probe entry: %v", err)
	}

	read := func() *pgss.Snapshot {
		t.Helper()
		s, err := pgss.Read(ctx, pool, pgss.ReadOptions{})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		return s
	}
	sampleFor := func(iv pgss.Interval) pgss.Sample {
		t.Helper()
		for _, s := range iv.Samples {
			if s.Key == key {
				return s
			}
		}
		t.Fatalf("no sample for probe query in %d samples", len(iv.Samples))
		return pgss.Sample{}
	}

	t.Run("steady", func(t *testing.T) {
		before := read()
		run(37)
		iv := pgss.Diff(before, read())
		s := sampleFor(iv)
		if s.Delta.Calls != 37 || s.Delta.Rows != 37 || s.Partial {
			t.Errorf("want 37 calls and 37 rows, not partial; got %+v", s)
		}
		if s.Delta.ExecMS <= 0 {
			t.Errorf("exec time should be positive, got %v", s.Delta.ExecMS)
		}
	})

	t.Run("single entry reset", func(t *testing.T) {
		before := read()
		mustExec(t, pool, "SELECT pg_stat_statements_reset($1, $2, $3)", key.UserID, key.DBID, key.QueryID)
		run(5)
		iv := pgss.Diff(before, read())
		s := sampleFor(iv)
		if s.Delta.Calls != 5 || !s.Partial || iv.Restarted == 0 {
			t.Errorf("want 5 partial calls and a restart; got %+v (restarted=%d)", s, iv.Restarted)
		}
	})

	t.Run("global reset", func(t *testing.T) {
		before := read()
		mustExec(t, pool, "SELECT pg_stat_statements_reset()")
		run(3)
		iv := pgss.Diff(before, read())
		if !iv.GlobalReset {
			t.Fatal("want GlobalReset")
		}
		if s := sampleFor(iv); s.Delta.Calls != 3 || !s.Partial {
			t.Errorf("want 3 partial calls; got %+v", s)
		}
	})

	t.Run("texts", func(t *testing.T) {
		texts, err := pgss.Texts(ctx, pool, []pgss.Key{key})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(texts[key], table) {
			t.Errorf("text %q does not mention %s", texts[key], table)
		}
	})
}

func TestIntegrationExcludesOwnRole(t *testing.T) {
	ctx := context.Background()
	pool := testpg.Pool(t)
	self, err := pgss.SelfOID(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	s, err := pgss.Read(ctx, pool, pgss.ReadOptions{ExcludeUserID: self})
	if err != nil {
		t.Fatal(err)
	}
	for k := range s.Entries {
		if k.UserID == self {
			t.Fatalf("entry for own role %d was not excluded", self)
		}
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
