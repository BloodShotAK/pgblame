package pgss

import (
	"cmp"
	"slices"
	"time"
)

// Since PG14 entries are keyed on all four fields.
type Key struct {
	UserID   uint32
	DBID     uint32
	QueryID  int64
	TopLevel bool
}

func (k Key) compare(o Key) int {
	return cmp.Or(
		cmp.Compare(k.DBID, o.DBID),
		cmp.Compare(k.UserID, o.UserID),
		cmp.Compare(k.QueryID, o.QueryID),
		boolCompare(k.TopLevel, o.TopLevel),
	)
}

func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}

// Only counters that can be subtracted; min/max/stddev are lifetime aggregates.
type Counters struct {
	Calls             int64
	Plans             int64
	ExecMS            float64
	PlanMS            float64
	Rows              int64
	SharedBlksHit     int64
	SharedBlksRead    int64
	SharedBlksDirtied int64
	SharedBlksWritten int64
	TempBlksRead      int64
	TempBlksWritten   int64
	WALBytes          int64
}

func (c Counters) sub(o Counters) Counters {
	return Counters{
		Calls:             c.Calls - o.Calls,
		Plans:             c.Plans - o.Plans,
		ExecMS:            c.ExecMS - o.ExecMS,
		PlanMS:            c.PlanMS - o.PlanMS,
		Rows:              c.Rows - o.Rows,
		SharedBlksHit:     c.SharedBlksHit - o.SharedBlksHit,
		SharedBlksRead:    c.SharedBlksRead - o.SharedBlksRead,
		SharedBlksDirtied: c.SharedBlksDirtied - o.SharedBlksDirtied,
		SharedBlksWritten: c.SharedBlksWritten - o.SharedBlksWritten,
		TempBlksRead:      c.TempBlksRead - o.TempBlksRead,
		TempBlksWritten:   c.TempBlksWritten - o.TempBlksWritten,
		WALBytes:          c.WALBytes - o.WALBytes,
	}
}

func (c Counters) decreasedFrom(o Counters) bool {
	d := c.sub(o)
	return d.Calls < 0 || d.Plans < 0 || d.ExecMS < 0 || d.PlanMS < 0 || d.Rows < 0 ||
		d.SharedBlksHit < 0 || d.SharedBlksRead < 0 || d.SharedBlksDirtied < 0 ||
		d.SharedBlksWritten < 0 || d.TempBlksRead < 0 || d.TempBlksWritten < 0 || d.WALBytes < 0
}

type Entry struct {
	Key
	Counters
	DBName   string
	UserName string
	// PG17+ only
	StatsSince time.Time
}

type Snapshot struct {
	// server clock, so it compares with StatsSince
	TakenAt          time.Time
	ServerVersionNum int
	StatsReset       time.Time
	Dealloc          int64
	Entries          map[Key]Entry
}

func (s *Snapshot) hasStatsSince() bool { return s.ServerVersionNum >= 170000 }

type Sample struct {
	Key
	Delta Counters
	// Partial means the entry was recreated mid-interval, so totals are a lower bound.
	Partial bool
}

type Interval struct {
	Start, End   time.Time
	Samples      []Sample
	GlobalReset  bool
	DeallocDelta int64
	// Before PG17, an entry that was evicted, recreated and has already outgrown
	// its old counters looks like normal growth, so the whole interval is flagged.
	EvictionSuspect bool
	Restarted       int
	Evicted         int
}

func Diff(prev, cur *Snapshot) Interval {
	iv := Interval{Start: prev.TakenAt, End: cur.TakenAt}
	// dealloc only goes backwards on a full reset or a crash that lost the stats file.
	iv.GlobalReset = !cur.StatsReset.Equal(prev.StatsReset) || cur.Dealloc < prev.Dealloc
	if !iv.GlobalReset {
		iv.DeallocDelta = cur.Dealloc - prev.Dealloc
		iv.EvictionSuspect = iv.DeallocDelta > 0 && !cur.hasStatsSince()
	}

	for k, c := range cur.Entries {
		p, seen := prev.Entries[k]
		s := Sample{Key: k}
		switch {
		case iv.GlobalReset:
			s.Delta, s.Partial = c.Counters, true
		case !seen:
			// Created after prev, so its whole life is inside this interval.
			s.Delta = c.Counters
		case recreated(p, c):
			s.Delta, s.Partial = c.Counters, true
			iv.Restarted++
		default:
			s.Delta = c.Counters.sub(p.Counters)
		}
		if s.Delta.Calls == 0 {
			continue
		}
		iv.Samples = append(iv.Samples, s)
	}

	for k := range prev.Entries {
		if _, ok := cur.Entries[k]; !ok {
			iv.Evicted++
		}
	}
	slices.SortFunc(iv.Samples, func(a, b Sample) int { return a.Key.compare(b.Key) })
	return iv
}

func recreated(prev, cur Entry) bool {
	if !prev.StatsSince.IsZero() && !cur.StatsSince.IsZero() {
		return !cur.StatsSince.Equal(prev.StatsSince)
	}
	return cur.Counters.decreasedFrom(prev.Counters)
}
