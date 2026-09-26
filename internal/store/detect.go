package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BloodShotAK/pgblame/internal/detect"
)

// Series returns detection input for every query of the target with complete
// samples in the window: raw intervals for the recent range, hourly rollups
// for prior weeks. Intervals with suspected evictions are dropped, and so is
// anything inside a past or ongoing regression of the same query, so a long
// regression never becomes its own baseline.
func (s *Store) Series(ctx context.Context, targetID int64, c detect.Config, now time.Time) (map[int64][]detect.Point, error) {
	var froms, tos []time.Time
	for _, r := range c.SeasonalRanges(now) {
		froms = append(froms, r.From)
		tos = append(tos, r.To)
	}
	recent := c.RecentRange(now)
	windowStart := now.Add(-c.Window)
	rows, err := s.pool.Query(ctx, `
		WITH active AS (
			SELECT DISTINCT s.query_id
			FROM query_samples s
			JOIN queries q ON q.id = s.query_id
			WHERE q.target_id = $1 AND s.interval_end > $2 AND s.interval_end <= $3 AND NOT s.partial
		),
		points AS (
			SELECT s.query_id, s.interval_start, s.interval_end, s.calls, s.exec_ms, s.rows,
			       s.shared_blks_hit, s.shared_blks_read
			FROM query_samples s
			JOIN active a ON a.query_id = s.query_id
			LEFT JOIN pgss_snapshots p ON p.target_id = $1 AND p.taken_at = s.interval_end
			WHERE s.interval_end > $4 AND s.interval_end <= $3
			  AND NOT s.partial AND p.eviction_suspect IS NOT TRUE
			UNION ALL
			SELECT h.query_id, h.hour_start, h.hour_start + interval '1 hour', h.calls, h.exec_ms, h.rows,
			       h.shared_blks_hit, h.shared_blks_read
			FROM query_samples_hourly h
			JOIN active a ON a.query_id = h.query_id
			WHERE EXISTS (SELECT 1 FROM unnest($5::timestamptz[], $6::timestamptz[]) r (lo, hi)
			              WHERE h.hour_start + interval '1 hour' > r.lo AND h.hour_start + interval '1 hour' <= r.hi)
		)
		SELECT * FROM points x
		WHERE x.interval_end > $2 OR NOT EXISTS (
		        SELECT 1 FROM regressions g
		        WHERE g.query_id = x.query_id
		          AND x.interval_end > g.opened_at - $7 * interval '1 second'
		          AND x.interval_start < coalesce(g.closed_at, 'infinity'))
		ORDER BY x.query_id, x.interval_end`,
		targetID, windowStart, now, recent.From, froms, tos, c.Window.Seconds())
	if err != nil {
		return nil, fmt.Errorf("loading series: %w", err)
	}
	defer rows.Close()
	out := map[int64][]detect.Point{}
	for rows.Next() {
		var id int64
		var p detect.Point
		if err := rows.Scan(&id, &p.Start, &p.End, &p.Calls, &p.ExecMS, &p.Rows, &p.BlksHit, &p.BlksRead); err != nil {
			return nil, err
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

// ApplyFindings keeps one open regression per query: findings open or
// refresh one, and queries judged healthy close theirs.
func (s *Store) ApplyFindings(ctx context.Context, at time.Time, found []detect.QueryFinding, healthy []int64) (opened, closed []int64, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, f := range found {
			var inserted bool
			// xmax is 0 only on rows this statement inserted rather than updated.
			err := tx.QueryRow(ctx, `
				INSERT INTO regressions (query_id, opened_at, last_seen_at, kind, baseline_kind,
				                         baseline_ms, current_ms, peak_ms, extra_ms_per_hour, calls, z)
				VALUES ($1, $2, $2, $3, $4, $5, $6, $6, $7, $8, $9)
				ON CONFLICT (query_id) WHERE closed_at IS NULL DO UPDATE SET
					last_seen_at      = excluded.last_seen_at,
					kind              = excluded.kind,
					current_ms        = excluded.current_ms,
					peak_ms           = greatest(regressions.peak_ms, excluded.current_ms),
					extra_ms_per_hour = excluded.extra_ms_per_hour,
					calls             = excluded.calls,
					z                 = excluded.z
				RETURNING xmax = 0`,
				f.QueryID, at, string(f.Kind), string(f.Baseline), f.BaselineMS, f.CurrentMS,
				float64(f.ExtraPerHour)/float64(time.Millisecond), f.Calls, f.Z).Scan(&inserted)
			if err != nil {
				return fmt.Errorf("recording regression for query %d: %w", f.QueryID, err)
			}
			if inserted {
				opened = append(opened, f.QueryID)
			}
		}
		rows, err := tx.Query(ctx, `
			UPDATE regressions SET closed_at = $2
			WHERE closed_at IS NULL AND query_id = ANY($1)
			RETURNING query_id`, healthy, at)
		if err != nil {
			return err
		}
		closed, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	return opened, closed, err
}

type QueryInfo struct {
	QueryID int64
	DBName  string
	Text    string
}

func (s *Store) QueryInfo(ctx context.Context, ids []int64) (map[int64]QueryInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, queryid, dbname, coalesce(query_text, '') FROM queries WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]QueryInfo{}
	for rows.Next() {
		var id int64
		var q QueryInfo
		if err := rows.Scan(&id, &q.QueryID, &q.DBName, &q.Text); err != nil {
			return nil, err
		}
		out[id] = q
	}
	return out, rows.Err()
}

func (s *Store) Targets(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, id FROM targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		out[name] = id
	}
	return out, rows.Err()
}

func (s *Store) LatestSnapshot(ctx context.Context, targetID int64) (time.Time, error) {
	var t *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT max(taken_at) FROM pgss_snapshots WHERE target_id = $1`, targetID).Scan(&t); err != nil {
		return time.Time{}, err
	}
	if t == nil {
		return time.Time{}, fmt.Errorf("nothing collected for this target yet")
	}
	return *t, nil
}
