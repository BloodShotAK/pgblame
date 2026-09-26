// Package detect decides whether a query got slower than its baseline. It
// only sees per-interval counters, so it doesn't depend on the database engine.
package detect

import (
	"cmp"
	"math"
	"slices"
	"time"
)

type Point struct {
	Start, End time.Time
	Calls      int64
	ExecMS     float64
	Rows       int64
	BlksHit    int64
	BlksRead   int64
}

type Kind string

const (
	// Same rows per call but far more blocks touched: the plan likely changed.
	MoreWork       Kind = "more_work"
	MoreRows       Kind = "more_rows"
	CacheMisses    Kind = "cache_misses"
	SlowerSameWork Kind = "slower_same_work"
)

type BaselineKind string

const (
	Seasonal BaselineKind = "prior_weeks"
	Trailing BaselineKind = "trailing"
)

type Config struct {
	Window   time.Duration
	Trailing time.Duration
	// Weeks of the same clock window to use as a baseline before falling
	// back to Trailing, so daily and weekly patterns aren't flagged.
	Weeks           int
	MinCalls        int64
	MinPoints       int
	MinRatio        float64
	MinZ            float64
	MinExtraPerHour time.Duration
}

func DefaultConfig() Config {
	return Config{
		Window:          15 * time.Minute,
		Trailing:        24 * time.Hour,
		Weeks:           4,
		MinCalls:        30,
		MinPoints:       5,
		MinRatio:        1.5,
		MinZ:            4,
		MinExtraPerHour: time.Second,
	}
}

// Wide enough that each prior week spans at least two hourly rollups.
const seasonalPad = time.Hour

// Range selects points by End, exclusive of From.
type Range struct{ From, To time.Time }

func (r Range) has(t time.Time) bool { return t.After(r.From) && !t.After(r.To) }

func (c Config) windowRange(now time.Time) Range { return Range{now.Add(-c.Window), now} }

func (c Config) trailingRange(now time.Time) Range {
	ws := now.Add(-c.Window)
	return Range{ws.Add(-c.Trailing), ws}
}

func (c Config) SeasonalRanges(now time.Time) []Range {
	var rs []Range
	for w := 1; w <= c.Weeks; w++ {
		off := time.Duration(w) * 7 * 24 * time.Hour
		rs = append(rs, Range{now.Add(-c.Window - off - seasonalPad), now.Add(-off + seasonalPad)})
	}
	return rs
}

// RecentRange covers the window and the trailing baseline before it.
func (c Config) RecentRange(now time.Time) Range {
	return Range{now.Add(-c.Window - c.Trailing), now}
}

type Finding struct {
	Kind           Kind
	Baseline       BaselineKind
	BaselineMS     float64
	CurrentMS      float64
	Ratio          float64
	Z              float64
	ExtraPerHour   time.Duration
	Calls          int64
	RowsRatio      float64
	BlocksRatio    float64
	ReadFracBefore float64
	ReadFracNow    float64
}

type Result struct {
	// Evaluated is false when there wasn't enough traffic to judge either way.
	Evaluated bool
	Finding   *Finding
}

type totals struct {
	calls, rows, hit, read int64
	execMS                 float64
	span                   time.Duration
}

func sum(ps []Point) totals {
	var t totals
	for _, p := range ps {
		t.calls += p.Calls
		t.execMS += p.ExecMS
		t.rows += p.Rows
		t.hit += p.BlksHit
		t.read += p.BlksRead
		t.span += p.End.Sub(p.Start)
	}
	return t
}

func (t totals) perCall(x int64) float64 { return float64(x) / float64(t.calls) }

func (t totals) readFrac() float64 {
	if t.hit+t.read == 0 {
		return 0
	}
	return float64(t.read) / float64(t.hit+t.read)
}

func filter(ps []Point, rs ...Range) []Point {
	var out []Point
	for _, p := range ps {
		for _, r := range rs {
			if r.has(p.End) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func (c Config) baseline(ps []Point, now time.Time) ([]Point, BaselineKind) {
	if b := filter(ps, c.SeasonalRanges(now)...); c.enough(b) {
		return b, Seasonal
	}
	return filter(ps, c.trailingRange(now)), Trailing
}

func (c Config) enough(ps []Point) bool {
	return len(ps) >= c.MinPoints && sum(ps).calls >= c.MinCalls
}

// Evaluate compares the per-call latency in the window ending at now with
// the median of per-interval latencies in the baseline. Median and MAD are
// used because latency is heavy-tailed and a few bad intervals in the
// baseline shouldn't move it.
func Evaluate(ps []Point, now time.Time, c Config) Result {
	win := sum(filter(ps, c.windowRange(now)))
	if win.calls < c.MinCalls || win.span <= 0 {
		return Result{}
	}
	basePoints, kind := c.baseline(ps, now)
	if !c.enough(basePoints) {
		return Result{}
	}

	var means []float64
	for _, p := range basePoints {
		if p.Calls > 0 {
			means = append(means, p.ExecMS/float64(p.Calls))
		}
	}
	median := medianOf(means)
	deviations := make([]float64, len(means))
	for i, m := range means {
		deviations[i] = math.Abs(m - median)
	}
	// A very steady baseline has a MAD near zero, which would turn any
	// wobble into a huge z-score.
	scale := max(1.4826*medianOf(deviations), 0.05*median, 0.001)

	cur := win.execMS / float64(win.calls)
	base := sum(basePoints)
	f := &Finding{
		Baseline:       kind,
		BaselineMS:     median,
		CurrentMS:      cur,
		Ratio:          ratio(cur, median),
		Z:              (cur - median) / scale,
		ExtraPerHour:   time.Duration((cur - median) * float64(win.calls) * float64(time.Hour) / float64(win.span) * float64(time.Millisecond)),
		Calls:          win.calls,
		RowsRatio:      ratio(win.perCall(win.rows), base.perCall(base.rows)),
		BlocksRatio:    ratio(win.perCall(win.hit+win.read), base.perCall(base.hit+base.read)),
		ReadFracBefore: base.readFrac(),
		ReadFracNow:    win.readFrac(),
	}
	if f.Ratio < c.MinRatio || f.Z < c.MinZ || f.ExtraPerHour < c.MinExtraPerHour {
		return Result{Evaluated: true}
	}
	f.Kind = classify(f)
	return Result{Evaluated: true, Finding: f}
}

func classify(f *Finding) Kind {
	switch {
	case f.RowsRatio >= 1.5 && f.Ratio <= f.RowsRatio*1.5:
		return MoreRows
	case f.BlocksRatio >= 2:
		return MoreWork
	case f.ReadFracNow-f.ReadFracBefore >= 0.2:
		return CacheMisses
	default:
		return SlowerSameWork
	}
}

type QueryFinding struct {
	QueryID int64
	Finding
}

// Scan evaluates every series and returns findings ranked by extra database
// time, plus the queries judged healthy. Queries with too little traffic
// are in neither.
func Scan(series map[int64][]Point, now time.Time, c Config) (found []QueryFinding, healthy []int64) {
	for id, ps := range series {
		r := Evaluate(ps, now, c)
		switch {
		case r.Finding != nil:
			found = append(found, QueryFinding{QueryID: id, Finding: *r.Finding})
		case r.Evaluated:
			healthy = append(healthy, id)
		}
	}
	slices.SortFunc(found, func(a, b QueryFinding) int {
		return cmp.Or(cmp.Compare(b.ExtraPerHour, a.ExtraPerHour), cmp.Compare(a.QueryID, b.QueryID))
	})
	slices.Sort(healthy)
	return found, healthy
}

func ratio(a, b float64) float64 {
	switch {
	case b > 0:
		return a / b
	case a == 0:
		return 1
	default:
		return math.Inf(1)
	}
}

func medianOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
