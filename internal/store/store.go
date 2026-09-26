package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BloodShotAK/pgculprit/internal/pgss"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) EnsureTarget(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO targets (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = excluded.name
		RETURNING id`, name).Scan(&id)
	return id, err
}

func (s *Store) QueryIDs(ctx context.Context, targetID int64) (ids, missingText map[pgss.Key]int64, err error) {
	rows, err := s.pool.Query(ctx, `SELECT dbid, userid, queryid, toplevel, id, query_text IS NULL FROM queries WHERE target_id = $1`, targetID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids, missingText = map[pgss.Key]int64{}, map[pgss.Key]int64{}
	for rows.Next() {
		var dbid, userid, id int64
		var k pgss.Key
		var noText bool
		if err := rows.Scan(&dbid, &userid, &k.QueryID, &k.TopLevel, &id, &noText); err != nil {
			return nil, nil, err
		}
		k.DBID, k.UserID = uint32(dbid), uint32(userid)
		ids[k] = id
		if noText {
			missingText[k] = id
		}
	}
	return ids, missingText, rows.Err()
}

func (s *Store) SetQueryTexts(ctx context.Context, texts map[int64]string) error {
	if len(texts) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(texts))
	vals := make([]string, 0, len(texts))
	for id, t := range texts {
		ids = append(ids, id)
		vals = append(vals, t)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE queries q SET query_text = t.text
		FROM unnest($1::bigint[], $2::text[]) t (id, text)
		WHERE q.id = t.id AND q.query_text IS NULL`, ids, vals)
	return err
}

type NewQuery struct {
	pgss.Entry
	Text *string
}

// AddQueries is idempotent, so retrying after a partial failure is safe.
func (s *Store) AddQueries(ctx context.Context, targetID int64, qs []NewQuery, seenAt time.Time) (map[pgss.Key]int64, error) {
	out := make(map[pgss.Key]int64, len(qs))
	if len(qs) == 0 {
		return out, nil
	}
	batch := &pgx.Batch{}
	for _, q := range qs {
		batch.Queue(`
			INSERT INTO queries (target_id, dbid, userid, queryid, toplevel, dbname, username, query_text, first_seen, last_seen)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
			ON CONFLICT (target_id, dbid, userid, queryid, toplevel)
			DO UPDATE SET last_seen = excluded.last_seen,
			              query_text = coalesce(queries.query_text, excluded.query_text)
			RETURNING id`,
			targetID, int64(q.DBID), int64(q.UserID), q.QueryID, q.TopLevel, q.DBName, q.UserName, q.Text, seenAt)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for _, q := range qs {
		var id int64
		if err := br.QueryRow().Scan(&id); err != nil {
			return nil, fmt.Errorf("recording query %d: %w", q.QueryID, err)
		}
		out[q.Key] = id
	}
	return out, nil
}

var sampleColumns = []string{
	"query_id", "interval_start", "interval_end", "partial",
	"calls", "plans", "exec_ms", "plan_ms", "rows",
	"shared_blks_hit", "shared_blks_read", "shared_blks_dirtied", "shared_blks_written",
	"temp_blks_read", "temp_blks_written", "wal_bytes",
}

// iv is nil for the first snapshot after startup.
func (s *Store) WriteSnapshot(ctx context.Context, targetID int64, snap *pgss.Snapshot, iv *pgss.Interval, ids map[pgss.Key]int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var statsReset *time.Time
		if !snap.StatsReset.IsZero() {
			statsReset = &snap.StatsReset
		}
		if iv == nil {
			_, err := tx.Exec(ctx, `
				INSERT INTO pgss_snapshots (target_id, taken_at, server_version_num, stats_reset, dealloc, entries)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				targetID, snap.TakenAt, snap.ServerVersionNum, statsReset, snap.Dealloc, len(snap.Entries))
			return err
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO pgss_snapshots (target_id, taken_at, server_version_num, stats_reset, dealloc, entries,
			                            interval_start, samples, global_reset, eviction_suspect, dealloc_delta, restarted, evicted)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			targetID, snap.TakenAt, snap.ServerVersionNum, statsReset, snap.Dealloc, len(snap.Entries),
			iv.Start, len(iv.Samples), iv.GlobalReset, iv.EvictionSuspect, iv.DeallocDelta, iv.Restarted, iv.Evicted)
		if err != nil {
			return fmt.Errorf("recording snapshot: %w", err)
		}

		rows := make([][]any, 0, len(iv.Samples))
		touched := make([]int64, 0, len(iv.Samples))
		for _, smp := range iv.Samples {
			id, ok := ids[smp.Key]
			if !ok {
				return fmt.Errorf("no recorded query for %+v", smp.Key)
			}
			d := smp.Delta
			rows = append(rows, []any{
				id, iv.Start, iv.End, smp.Partial,
				d.Calls, d.Plans, d.ExecMS, d.PlanMS, d.Rows,
				d.SharedBlksHit, d.SharedBlksRead, d.SharedBlksDirtied, d.SharedBlksWritten,
				d.TempBlksRead, d.TempBlksWritten, d.WALBytes,
			})
			touched = append(touched, id)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"query_samples"}, sampleColumns, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("writing samples: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE queries SET last_seen = $2 WHERE id = ANY($1)`, touched, iv.End)
		return err
	})
}
