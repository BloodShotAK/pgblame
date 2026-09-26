package pgss

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PG14 added pg_stat_statements_info and the toplevel key, both needed to
// tell resets and evictions apart from real activity.
const MinServerVersionNum = 140000

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type ReadOptions struct {
	ExcludeUserID uint32
}

// Read skips query texts: reading the extension's text file every poll is
// expensive, so Texts fetches them only for new entries.
func Read(ctx context.Context, q Querier, opts ReadOptions) (*Snapshot, error) {
	var version int
	if err := q.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		return nil, fmt.Errorf("reading server version: %w", err)
	}
	if version < MinServerVersionNum {
		return nil, fmt.Errorf("server version %d is older than PostgreSQL 14, which pgblame requires", version)
	}

	s := &Snapshot{ServerVersionNum: version, Entries: map[Key]Entry{}}
	var statsReset *time.Time
	err := q.QueryRow(ctx,
		`SELECT clock_timestamp(), dealloc, stats_reset FROM pg_stat_statements_info`,
	).Scan(&s.TakenAt, &s.Dealloc, &statsReset)
	if err != nil {
		return nil, fmt.Errorf("reading pg_stat_statements_info: %w", err)
	}
	if statsReset != nil {
		s.StatsReset = *statsReset
	}

	statsSince := "NULL::timestamptz"
	if s.hasStatsSince() {
		statsSince = "s.stats_since"
	}
	// queryid is NULL for rows we aren't allowed to see.
	rows, err := q.Query(ctx, `
		SELECT s.userid, s.dbid, s.queryid, s.toplevel,
		       coalesce(d.datname, ''), coalesce(r.rolname, ''),
		       s.calls, s.plans, s.total_exec_time, s.total_plan_time, s.rows,
		       s.shared_blks_hit, s.shared_blks_read, s.shared_blks_dirtied, s.shared_blks_written,
		       s.temp_blks_read, s.temp_blks_written, s.wal_bytes::bigint,
		       `+statsSince+`
		FROM pg_stat_statements(false) s
		LEFT JOIN pg_database d ON d.oid = s.dbid
		LEFT JOIN pg_roles r ON r.oid = s.userid
		WHERE s.queryid IS NOT NULL
		  AND ($1::oid = 0 OR s.userid <> $1::oid)`,
		opts.ExcludeUserID)
	if err != nil {
		return nil, fmt.Errorf("reading pg_stat_statements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var since *time.Time
		err := rows.Scan(&e.UserID, &e.DBID, &e.QueryID, &e.TopLevel, &e.DBName, &e.UserName,
			&e.Calls, &e.Plans, &e.ExecMS, &e.PlanMS, &e.Rows,
			&e.SharedBlksHit, &e.SharedBlksRead, &e.SharedBlksDirtied, &e.SharedBlksWritten,
			&e.TempBlksRead, &e.TempBlksWritten, &e.WALBytes, &since)
		if err != nil {
			return nil, fmt.Errorf("scanning pg_stat_statements row: %w", err)
		}
		if since != nil {
			e.StatsSince = *since
		}
		s.Entries[e.Key] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading pg_stat_statements: %w", err)
	}
	return s, nil
}

// Texts can be missing once the extension garbage-collects its text file.
func Texts(ctx context.Context, q Querier, keys []Key) (map[Key]string, error) {
	out := make(map[Key]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	want := make(map[Key]bool, len(keys))
	ids := make([]int64, 0, len(keys))
	for _, k := range keys {
		want[k] = true
		ids = append(ids, k.QueryID)
	}
	rows, err := q.Query(ctx, `
		SELECT userid, dbid, queryid, toplevel, query
		FROM pg_stat_statements(true)
		WHERE queryid = ANY($1) AND query IS NOT NULL`, ids)
	if err != nil {
		return nil, fmt.Errorf("reading query texts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k Key
		var text string
		if err := rows.Scan(&k.UserID, &k.DBID, &k.QueryID, &k.TopLevel, &text); err != nil {
			return nil, fmt.Errorf("scanning query text: %w", err)
		}
		if want[k] {
			out[k] = text
		}
	}
	return out, rows.Err()
}

func SelfOID(ctx context.Context, q Querier) (uint32, error) {
	var oid uint32
	err := q.QueryRow(ctx, `SELECT current_user::regrole::oid`).Scan(&oid)
	return oid, err
}
