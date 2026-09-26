package pgss

import (
	"testing"
	"time"
)

var (
	t0    = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	t1    = t0.Add(time.Minute)
	reset = t0.Add(-24 * time.Hour)
	keyA  = Key{UserID: 10, DBID: 5, QueryID: 111, TopLevel: true}
	keyB  = Key{UserID: 10, DBID: 5, QueryID: 222, TopLevel: true}
)

func entry(k Key, calls int64, execMS float64) Entry {
	return Entry{Key: k, Counters: Counters{Calls: calls, ExecMS: execMS, Rows: calls * 2}}
}

func snap(at time.Time, version int, dealloc int64, entries ...Entry) *Snapshot {
	s := &Snapshot{TakenAt: at, ServerVersionNum: version, StatsReset: reset, Dealloc: dealloc, Entries: map[Key]Entry{}}
	for _, e := range entries {
		s.Entries[e.Key] = e
	}
	return s
}

func onlySample(t *testing.T, iv Interval) Sample {
	t.Helper()
	if len(iv.Samples) != 1 {
		t.Fatalf("want 1 sample, got %d: %+v", len(iv.Samples), iv.Samples)
	}
	return iv.Samples[0]
}

func TestDiffNormalGrowth(t *testing.T) {
	iv := Diff(
		snap(t0, 160000, 0, entry(keyA, 100, 50)),
		snap(t1, 160000, 0, entry(keyA, 130, 80)),
	)
	s := onlySample(t, iv)
	if s.Delta.Calls != 30 || s.Delta.ExecMS != 30 || s.Delta.Rows != 60 || s.Partial {
		t.Errorf("unexpected sample %+v", s)
	}
	if !iv.Start.Equal(t0) || !iv.End.Equal(t1) {
		t.Errorf("interval bounds %v..%v", iv.Start, iv.End)
	}
	if iv.GlobalReset || iv.EvictionSuspect || iv.Restarted != 0 || iv.Evicted != 0 {
		t.Errorf("unexpected flags %+v", iv)
	}
}

func TestDiffIdleEntryEmitsNothing(t *testing.T) {
	iv := Diff(
		snap(t0, 160000, 0, entry(keyA, 100, 50)),
		snap(t1, 160000, 0, entry(keyA, 100, 50)),
	)
	if len(iv.Samples) != 0 {
		t.Errorf("idle entry produced samples: %+v", iv.Samples)
	}
}

func TestDiffNewEntryUsesFullCounters(t *testing.T) {
	iv := Diff(
		snap(t0, 160000, 0, entry(keyA, 100, 50)),
		snap(t1, 160000, 0, entry(keyA, 100, 50), entry(keyB, 7, 3.5)),
	)
	s := onlySample(t, iv)
	if s.Key != keyB || s.Delta.Calls != 7 || s.Partial {
		t.Errorf("new entry should count in full and not be partial, got %+v", s)
	}
}

func TestDiffCounterDecreaseMeansRecreated(t *testing.T) {
	iv := Diff(
		snap(t0, 160000, 0, entry(keyA, 100, 50)),
		snap(t1, 160000, 1, entry(keyA, 4, 2)),
	)
	s := onlySample(t, iv)
	if s.Delta.Calls != 4 || !s.Partial {
		t.Errorf("recreated entry should report its current counters as partial, got %+v", s)
	}
	if iv.Restarted != 1 {
		t.Errorf("Restarted = %d, want 1", iv.Restarted)
	}
}

func TestDiffStatsSinceCatchesRecreationCountersMiss(t *testing.T) {
	before := entry(keyA, 100, 50)
	before.StatsSince = t0.Add(-time.Hour)
	after := entry(keyA, 150, 90)
	after.StatsSince = t0.Add(30 * time.Second)

	iv := Diff(snap(t0, 170000, 0, before), snap(t1, 170000, 1, after))
	s := onlySample(t, iv)
	if s.Delta.Calls != 150 || !s.Partial || iv.Restarted != 1 {
		t.Errorf("want partial sample with 150 calls, got %+v (restarted=%d)", s, iv.Restarted)
	}
	if iv.EvictionSuspect {
		t.Error("PG17 detects recreation exactly and should not flag the interval")
	}
}

func TestDiffEvictionSuspectBeforePG17(t *testing.T) {
	iv := Diff(
		snap(t0, 160000, 3, entry(keyA, 100, 50)),
		snap(t1, 160000, 5, entry(keyA, 150, 90)),
	)
	s := onlySample(t, iv)
	if s.Partial || iv.Restarted != 0 {
		t.Errorf("counter growth is indistinguishable from normal activity, got %+v", s)
	}
	if !iv.EvictionSuspect || iv.DeallocDelta != 2 {
		t.Errorf("want EvictionSuspect with DeallocDelta 2, got %+v", iv)
	}
}

func TestDiffGlobalReset(t *testing.T) {
	prev := snap(t0, 160000, 9, entry(keyA, 100, 50), entry(keyB, 10, 1))
	cur := snap(t1, 160000, 0, entry(keyA, 3, 1.5))
	cur.StatsReset = t0.Add(20 * time.Second)

	iv := Diff(prev, cur)
	if !iv.GlobalReset {
		t.Fatal("want GlobalReset")
	}
	s := onlySample(t, iv)
	if s.Delta.Calls != 3 || !s.Partial {
		t.Errorf("after a reset counters are all from this interval and partial, got %+v", s)
	}
	if iv.DeallocDelta != 0 || iv.Evicted != 1 {
		t.Errorf("DeallocDelta=%d Evicted=%d, want 0 and 1", iv.DeallocDelta, iv.Evicted)
	}
}

func TestDiffDeallocGoingBackwardsIsAReset(t *testing.T) {
	iv := Diff(
		snap(t0, 160000, 9, entry(keyA, 100, 50)),
		snap(t1, 160000, 0, entry(keyA, 120, 60)),
	)
	if !iv.GlobalReset {
		t.Fatal("dealloc decreasing should be treated as a global reset")
	}
	if s := onlySample(t, iv); s.Delta.Calls != 120 || !s.Partial {
		t.Errorf("got %+v", s)
	}
}

func TestDiffCountsEvictedAndSortsSamples(t *testing.T) {
	gone := Key{UserID: 10, DBID: 5, QueryID: 999, TopLevel: true}
	iv := Diff(
		snap(t0, 160000, 0, entry(keyB, 1, 1), entry(keyA, 1, 1), entry(gone, 5, 5)),
		snap(t1, 160000, 1, entry(keyB, 2, 2), entry(keyA, 2, 2)),
	)
	if iv.Evicted != 1 {
		t.Errorf("Evicted = %d, want 1", iv.Evicted)
	}
	if len(iv.Samples) != 2 || iv.Samples[0].Key != keyA || iv.Samples[1].Key != keyB {
		t.Errorf("samples not sorted by key: %+v", iv.Samples)
	}
}
