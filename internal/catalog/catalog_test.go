package catalog

import (
	"testing"
)

func ptr(f float64) *float64 { return &f }

func TestDiff(t *testing.T) {
	recorded := map[string]int{"kept": 1, "changed": 2, "dropped": 3}
	observed := map[string]int{"kept": 1, "changed": 20, "new": 4}

	got := map[string]Change[string, int]{}
	for _, c := range Diff(recorded, observed, func(a, b int) bool { return a != b }) {
		got[c.Key] = c
	}
	want := map[string]Change[string, int]{
		"changed": {Kind: Modified, Key: "changed", Old: 2, New: 20},
		"dropped": {Kind: Removed, Key: "dropped", Old: 3},
		"new":     {Kind: Added, Key: "new", New: 4},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %+v, want %+v", k, got[k], w)
		}
	}
}

func TestStatsShifted(t *testing.T) {
	base := ColumnStats{NullFrac: 0.05, NDistinct: 1000, Correlation: ptr(0.9)}
	with := func(f func(*ColumnStats)) ColumnStats {
		s := base
		f(&s)
		return s
	}
	cases := []struct {
		name string
		cur  ColumnStats
		want bool
	}{
		{"identical", base, false},
		{"routine analyze noise", with(func(s *ColumnStats) { s.NullFrac = 0.06; s.NDistinct = 1150; s.Correlation = ptr(0.85) }), false},
		{"null fraction jump", with(func(s *ColumnStats) { s.NullFrac = 0.4 }), true},
		{"n_distinct doubled", with(func(s *ColumnStats) { s.NDistinct = 2000 }), true},
		{"n_distinct halved", with(func(s *ColumnStats) { s.NDistinct = 500 }), true},
		{"n_distinct sign flip", with(func(s *ColumnStats) { s.NDistinct = -0.3 }), true},
		{"n_distinct became unknown", with(func(s *ColumnStats) { s.NDistinct = 0 }), true},
		{"correlation collapsed", with(func(s *ColumnStats) { s.Correlation = ptr(0.1) }), true},
		{"correlation disappeared", with(func(s *ColumnStats) { s.Correlation = nil }), true},
	}
	for _, c := range cases {
		if got := StatsShifted(base, c.cur); got != c.want {
			t.Errorf("%s: StatsShifted = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNDistinctFractionsCompareByRatio(t *testing.T) {
	if nDistinctShifted(-0.5, -0.4) {
		t.Error("-0.5 to -0.4 is a small change in the same representation")
	}
	if !nDistinctShifted(-0.5, -0.1) {
		t.Error("-0.5 to -0.1 is a 5x change")
	}
}
