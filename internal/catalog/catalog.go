// Package catalog tracks the parts of the catalog that decide query plans.
package catalog

import (
	"math"
	"time"
)

type IndexKey struct {
	Schema, Table, Index string
}

type Index struct {
	Definition string
	// false after a failed CREATE INDEX CONCURRENTLY; the planner ignores it
	Valid   bool
	Ready   bool
	Unique  bool
	Primary bool
}

type Setting struct {
	Value  string
	Unit   string
	Source string
}

type ColumnKey struct {
	Schema, Table, Column string
	Inherited             bool
}

// Histograms and most-common values are skipped because they contain real data.
type ColumnStats struct {
	NullFrac float64
	// > 0 is a count, < 0 is minus a fraction of rows
	NDistinct   float64
	Correlation *float64
}

type TableStats struct {
	Schema, Table   string
	RelTuples       int64
	RelPages        int32
	LiveTuples      int64
	DeadTuples      int64
	LastAnalyze     *time.Time
	LastAutoanalyze *time.Time
}

type Snapshot struct {
	TakenAt         time.Time
	DBName          string
	Indexes         map[IndexKey]Index
	Settings        map[string]Setting
	Columns         map[ColumnKey]ColumnStats
	ColumnsComplete bool
	Tables          []TableStats
}

type ChangeKind string

const (
	Added    ChangeKind = "added"
	Removed  ChangeKind = "removed"
	Modified ChangeKind = "modified"
)

type Change[K comparable, V any] struct {
	Kind     ChangeKind
	Key      K
	Old, New V
}

func Diff[K comparable, V any](recorded, observed map[K]V, changed func(old, new V) bool) []Change[K, V] {
	var out []Change[K, V]
	for k, nv := range observed {
		ov, ok := recorded[k]
		switch {
		case !ok:
			out = append(out, Change[K, V]{Kind: Added, Key: k, New: nv})
		case changed(ov, nv):
			out = append(out, Change[K, V]{Kind: Modified, Key: k, Old: ov, New: nv})
		}
	}
	for k, ov := range recorded {
		if _, ok := observed[k]; !ok {
			out = append(out, Change[K, V]{Kind: Removed, Key: k, Old: ov})
		}
	}
	return out
}

// StatsShifted ignores routine ANALYZE noise. Callers compare against the last
// recorded version, so slow drift still gets recorded once it adds up.
func StatsShifted(old, cur ColumnStats) bool {
	if math.Abs(cur.NullFrac-old.NullFrac) >= 0.1 {
		return true
	}
	if nDistinctShifted(old.NDistinct, cur.NDistinct) {
		return true
	}
	switch {
	case (old.Correlation == nil) != (cur.Correlation == nil):
		return true
	case old.Correlation != nil && math.Abs(*cur.Correlation-*old.Correlation) >= 0.3:
		return true
	}
	return false
}

func nDistinctShifted(old, cur float64) bool {
	if old == cur {
		return false
	}
	// 0 is unknown; a sign flip means ANALYZE switched between count and fraction.
	if old == 0 || cur == 0 || (old < 0) != (cur < 0) {
		return true
	}
	a, b := math.Abs(old), math.Abs(cur)
	return math.Max(a, b)/math.Min(a, b) >= 2
}
