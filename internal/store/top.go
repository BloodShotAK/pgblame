package store

import (
	"context"
	"time"
)

type TopQuery struct {
	QueryID  int64
	DBName   string
	Calls    int64
	TotalMS  float64
	MeanMS   float64
	RowsCall float64
	Text     string
}

// Partial samples are left out so call counts stay exact.
func (s *Store) Top(ctx context.Context, since time.Time, limit int) ([]TopQuery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT q.queryid, q.dbname, sum(s.calls), sum(s.exec_ms),
		       sum(s.exec_ms) / sum(s.calls), sum(s.rows)::float8 / sum(s.calls),
		       coalesce(q.query_text, '')
		FROM query_samples s
		JOIN queries q ON q.id = s.query_id
		WHERE s.interval_end > $1 AND NOT s.partial
		GROUP BY q.id
		ORDER BY sum(s.exec_ms) DESC
		LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TopQuery
	for rows.Next() {
		var t TopQuery
		if err := rows.Scan(&t.QueryID, &t.DBName, &t.Calls, &t.TotalMS, &t.MeanMS, &t.RowsCall, &t.Text); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
