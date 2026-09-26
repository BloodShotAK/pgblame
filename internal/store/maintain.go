package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Retention struct {
	Raw    time.Duration
	Hourly time.Duration
}

func DefaultRetention() Retention {
	return Retention{Raw: 3 * 24 * time.Hour, Hourly: 35 * 24 * time.Hour}
}

type MaintainResult struct {
	Skipped         bool
	RolledUp        int64
	DroppedDays     []string
	DeletedHourly   int64
	DeletedSnapshot int64
}

const maintainLock = migrationLock + 1

// Maintain keeps the store bounded: it creates upcoming daily partitions,
// rolls finished hours up, and drops data past retention. Several collectors
// can share a store; only one maintains at a time and the rest skip.
func (s *Store) Maintain(ctx context.Context, now time.Time, r Retention) (MaintainResult, error) {
	var res MaintainResult
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return res, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(maintainLock)).Scan(&locked); err != nil {
		return res, err
	}
	if !locked {
		res.Skipped = true
		return res, nil
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(maintainLock))

	now = now.UTC()
	today := now.Truncate(24 * time.Hour)
	for d := -1; d <= 2; d++ {
		day := today.AddDate(0, 0, d)
		_, err := conn.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF query_samples FOR VALUES FROM ('%s') TO ('%s')`,
			partitionName(day), day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339)))
		if err != nil {
			return res, fmt.Errorf("creating partition for %s: %w", day.Format(time.DateOnly), err)
		}
	}

	// Recomputing the last few finished hours makes the rollup idempotent
	// and covers a missed run.
	hour := now.Truncate(time.Hour)
	tag, err := conn.Exec(ctx, `
		INSERT INTO query_samples_hourly (query_id, hour_start, calls, exec_ms, rows, shared_blks_hit, shared_blks_read)
		SELECT s.query_id, date_trunc('hour', s.interval_start), sum(s.calls), sum(s.exec_ms), sum(s.rows),
		       sum(s.shared_blks_hit), sum(s.shared_blks_read)
		FROM query_samples s
		JOIN queries q ON q.id = s.query_id
		LEFT JOIN pgss_snapshots p ON p.target_id = q.target_id AND p.taken_at = s.interval_end
		WHERE s.interval_start >= $1 AND s.interval_start < $2
		  AND NOT s.partial AND p.eviction_suspect IS NOT TRUE
		GROUP BY 1, 2
		ON CONFLICT (query_id, hour_start) DO UPDATE SET
			calls = excluded.calls, exec_ms = excluded.exec_ms, rows = excluded.rows,
			shared_blks_hit = excluded.shared_blks_hit, shared_blks_read = excluded.shared_blks_read`,
		hour.Add(-3*time.Hour), hour)
	if err != nil {
		return res, fmt.Errorf("rolling up: %w", err)
	}
	res.RolledUp = tag.RowsAffected()

	rows, err := conn.Query(ctx, `
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'query_samples'::regclass`)
	if err != nil {
		return res, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return res, err
	}
	cutoff := now.Add(-r.Raw)
	for _, name := range names {
		day, err := time.Parse("20060102", strings.TrimPrefix(name, "query_samples_"))
		if err != nil || !day.AddDate(0, 0, 1).Before(cutoff) {
			continue
		}
		if _, err := conn.Exec(ctx, `DROP TABLE `+pgx.Identifier{name}.Sanitize()); err != nil {
			return res, fmt.Errorf("dropping %s: %w", name, err)
		}
		res.DroppedDays = append(res.DroppedDays, day.Format(time.DateOnly))
	}

	if tag, err = conn.Exec(ctx, `DELETE FROM query_samples_hourly WHERE hour_start < $1`, now.Add(-r.Hourly)); err != nil {
		return res, err
	}
	res.DeletedHourly = tag.RowsAffected()
	if tag, err = conn.Exec(ctx, `DELETE FROM pgss_snapshots WHERE taken_at < $1`, cutoff); err != nil {
		return res, err
	}
	res.DeletedSnapshot = tag.RowsAffected()
	return res, nil
}

func partitionName(day time.Time) string { return "query_samples_" + day.Format("20060102") }
