package detect

import (
	"testing"
	"time"
)

var now = time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)

type shape struct {
	ms, jitter         float64
	calls              int64
	rowsPer, blocksPer float64
	readFrac           float64
}

var normal = shape{ms: 2, jitter: 0.05, calls: 100, rowsPer: 1, blocksPer: 4}

// minutes emits one point per minute ending at each minute in (from, to].
func minutes(from, to time.Time, s shape) []Point {
	var ps []Point
	i := 0
	for end := from.Add(time.Minute); !end.After(to); end = end.Add(time.Minute) {
		ms := s.ms
		if i%2 == 0 {
			ms *= 1 + s.jitter
		} else {
			ms *= 1 - s.jitter
		}
		i++
		blocks := int64(s.blocksPer * float64(s.calls))
		read := int64(float64(blocks) * s.readFrac)
		ps = append(ps, Point{
			Start: end.Add(-time.Minute), End: end, Calls: s.calls,
			ExecMS: ms * float64(s.calls), Rows: int64(s.rowsPer * float64(s.calls)),
			BlksHit: blocks - read, BlksRead: read,
		})
	}
	return ps
}

func series(window shape) []Point {
	ws := now.Add(-15 * time.Minute)
	return append(minutes(ws.Add(-2*time.Hour), ws, normal), minutes(ws, now, window)...)
}

func evaluate(t *testing.T, ps []Point) Result {
	t.Helper()
	return Evaluate(ps, now, DefaultConfig())
}

func TestSteadyQueryIsHealthy(t *testing.T) {
	r := evaluate(t, series(normal))
	if !r.Evaluated || r.Finding != nil {
		t.Fatalf("want evaluated and healthy, got %+v", r)
	}
}

func TestClassification(t *testing.T) {
	cases := []struct {
		name   string
		window shape
		want   Kind
	}{
		{"index dropped", shape{ms: 40, calls: 100, rowsPer: 1, blocksPer: 1500}, MoreWork},
		{"more rows per call", shape{ms: 16, calls: 100, rowsPer: 10, blocksPer: 40}, MoreRows},
		{"cache went cold", shape{ms: 6, calls: 100, rowsPer: 1, blocksPer: 4, readFrac: 0.6}, CacheMisses},
		{"slower, same work", shape{ms: 6, calls: 100, rowsPer: 1, blocksPer: 4}, SlowerSameWork},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := evaluate(t, series(c.window))
			if r.Finding == nil {
				t.Fatalf("want a finding, got %+v", r)
			}
			f := r.Finding
			if f.Kind != c.want {
				t.Errorf("kind = %s, want %s (%+v)", f.Kind, c.want, f)
			}
			if f.Baseline != Trailing || f.BaselineMS != 2 || f.Calls != 1500 {
				t.Errorf("unexpected baseline or calls: %+v", f)
			}
		})
	}
}

func TestExtraPerHour(t *testing.T) {
	// 4ms extra per call at 100 calls a minute is 24s of extra time per hour.
	r := evaluate(t, series(shape{ms: 6, calls: 100, rowsPer: 1, blocksPer: 4}))
	if r.Finding == nil || r.Finding.ExtraPerHour != 24*time.Second {
		t.Fatalf("want 24s extra per hour, got %+v", r.Finding)
	}
}

func TestNotEnoughTrafficIsNotJudged(t *testing.T) {
	quiet := normal
	quiet.calls = 1
	quiet.ms = 50
	if r := evaluate(t, series(quiet)); r.Evaluated {
		t.Fatalf("15 calls in the window is too few to judge, got %+v", r)
	}
	if r := Evaluate(minutes(now.Add(-15*time.Minute), now, normal), now, DefaultConfig()); r.Evaluated {
		t.Fatalf("no baseline should mean no verdict, got %+v", r)
	}
}

func TestNoisyBaselineNeedsABiggerShift(t *testing.T) {
	ws := now.Add(-15 * time.Minute)
	noisy := shape{ms: 2, jitter: 0.5, calls: 100, rowsPer: 1, blocksPer: 4}
	ps := append(minutes(ws.Add(-2*time.Hour), ws, noisy), minutes(ws, now, shape{ms: 3.2, calls: 100, rowsPer: 1, blocksPer: 4})...)
	r := Evaluate(ps, now, DefaultConfig())
	if !r.Evaluated || r.Finding != nil {
		t.Fatalf("1.6x is within the noise of a baseline swinging 1-3ms, got %+v", r.Finding)
	}
}

func TestTinyAbsoluteImpactIsIgnored(t *testing.T) {
	ws := now.Add(-15 * time.Minute)
	fast := shape{ms: 0.01, jitter: 0.05, calls: 40, rowsPer: 1, blocksPer: 1}
	slower := fast
	slower.ms = 0.05
	ps := append(minutes(ws.Add(-2*time.Hour), ws, fast), minutes(ws, now, slower)...)
	if r := Evaluate(ps, now, DefaultConfig()); r.Finding != nil {
		t.Fatalf("5x of 10µs costs under a second an hour, got %+v", r.Finding)
	}
}

// A query that is always slow at this hour (say, during a nightly batch)
// shouldn't be flagged just because the last day was quieter.
func TestPriorWeeksBaselineAbsorbsRecurringPatterns(t *testing.T) {
	batch := shape{ms: 10, jitter: 0.05, calls: 100, rowsPer: 1, blocksPer: 4}
	ps := series(batch)
	for w := 1; w <= 4; w++ {
		off := time.Duration(w) * 7 * 24 * time.Hour
		ps = append(ps, minutes(now.Add(-off-45*time.Minute), now.Add(-off+15*time.Minute), batch)...)
	}

	r := Evaluate(ps, now, DefaultConfig())
	if !r.Evaluated || r.Finding != nil {
		t.Fatalf("slow at this hour every week is normal, got %+v", r.Finding)
	}

	noHistory := DefaultConfig()
	noHistory.Weeks = 0
	if r := Evaluate(ps, now, noHistory); r.Finding == nil {
		t.Fatal("against the trailing day alone it should look like a regression")
	}
}

func TestScanRanksByExtraTime(t *testing.T) {
	series := map[int64][]Point{
		1: series(normal),
		2: series(shape{ms: 6, calls: 100, rowsPer: 1, blocksPer: 4}),
		3: series(shape{ms: 40, calls: 100, rowsPer: 1, blocksPer: 1500}),
		4: minutes(now.Add(-15*time.Minute), now, normal),
	}
	found, healthy := Scan(series, now, DefaultConfig())
	if len(found) != 2 || found[0].QueryID != 3 || found[1].QueryID != 2 {
		t.Errorf("want queries 3 then 2, got %+v", found)
	}
	if len(healthy) != 1 || healthy[0] != 1 {
		t.Errorf("want only query 1 healthy (4 has no baseline), got %v", healthy)
	}
}

func TestRangesCoverEverythingEvaluateReads(t *testing.T) {
	c := DefaultConfig()
	rs := append([]Range{c.RecentRange(now)}, c.SeasonalRanges(now)...)
	covered := func(r Range) bool {
		for _, x := range rs {
			if !r.From.Before(x.From) && !r.To.After(x.To) {
				return true
			}
		}
		return false
	}
	for _, r := range append([]Range{c.windowRange(now), c.trailingRange(now)}, c.SeasonalRanges(now)...) {
		if !covered(r) {
			t.Errorf("range %v..%v is read but not fetched", r.From, r.To)
		}
	}
}
