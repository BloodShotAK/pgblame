// Command harness measures pgculprit's detection against injected faults. It
// creates (and drops) its own database and role on the target server, so
// point it at a disposable server only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BloodShotAK/pgculprit/internal/collector"
	"github.com/BloodShotAK/pgculprit/internal/detect"
	"github.com/BloodShotAK/pgculprit/internal/store"
)

type options struct {
	adminDSN, storeDSN string
	rate               int
	baseline, fault    time.Duration
	interval           time.Duration
	only               string
	verbose, strict    bool
}

type outcome struct {
	scenario string
	expect   map[string]detect.Kind
	flagged  map[string]detect.Finding
}

func main() {
	var o options
	flag.StringVar(&o.adminDSN, "target-dsn", os.Getenv("PGCULPRIT_TARGET_DSN"), "superuser DSN for a disposable server")
	flag.StringVar(&o.storeDSN, "store-dsn", os.Getenv("PGCULPRIT_STORE_DSN"), "store DSN")
	flag.IntVar(&o.rate, "rate", 10, "calls per second for each workload query")
	flag.DurationVar(&o.baseline, "baseline", time.Minute, "healthy period before each fault")
	flag.DurationVar(&o.fault, "fault", 40*time.Second, "period after each fault")
	flag.DurationVar(&o.interval, "interval", 3*time.Second, "collector poll interval")
	flag.StringVar(&o.only, "only", "", "comma-separated scenarios to run")
	flag.BoolVar(&o.verbose, "v", false, "show collector logs")
	flag.BoolVar(&o.strict, "strict", false, "exit non-zero on any miss or false alarm")
	yes := flag.Bool("yes", false, "confirm the target server is disposable")
	flag.Parse()
	if !*yes {
		fmt.Fprintf(os.Stderr, "harness drops and recreates database %s and role harness_app on the target; rerun with -yes\n", dbName)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results, err := run(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
	failed := report(os.Stdout, results)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0); err == nil {
			report(f, results)
			f.Close()
		}
	}
	if failed && o.strict {
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) ([]outcome, error) {
	admin, err := pgxpool.New(ctx, o.adminDSN)
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	if err := setup(ctx, admin); err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}

	targetCfg := admin.Config().Copy()
	targetCfg.ConnConfig.Database = dbName
	target, err := pgxpool.NewWithConfig(ctx, targetCfg)
	if err != nil {
		return nil, err
	}
	defer target.Close()
	for _, stmt := range append([]string{`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`}, schema...) {
		if _, err := target.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("creating schema: %w", err)
		}
	}

	appCfg := targetCfg.Copy()
	appCfg.ConnConfig.User, appCfg.ConnConfig.Password = "harness_app", "harness"
	app, err := pgxpool.NewWithConfig(ctx, appCfg)
	if err != nil {
		return nil, err
	}
	defer app.Close()

	storePool, err := pgxpool.New(ctx, o.storeDSN)
	if err != nil {
		return nil, err
	}
	defer storePool.Close()
	st := store.New(storePool)
	if err := st.Migrate(ctx); err != nil {
		return nil, err
	}

	var results []outcome
	for _, sc := range scenarios {
		if o.only != "" && !slices.Contains(strings.Split(o.only, ","), sc.name) {
			continue
		}
		fmt.Fprintf(os.Stderr, "running %s (%s)\n", sc.name, o.baseline+o.fault)
		res, err := runScenario(ctx, o, sc, target, app, st)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sc.name, err)
		}
		results = append(results, res)
	}
	return results, nil
}

func setup(ctx context.Context, admin *pgxpool.Pool) error {
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + dbName + ` WITH (FORCE)`,
		`CREATE DATABASE ` + dbName,
		`DO $$ BEGIN
		   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'harness_app') THEN
		     CREATE ROLE harness_app LOGIN PASSWORD 'harness';
		   END IF;
		 END $$`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func runScenario(ctx context.Context, o options, sc scenario, target, app *pgxpool.Pool, st *store.Store) (outcome, error) {
	name := fmt.Sprintf("harness/%s/%d", sc.name, time.Now().Unix())
	logs := io.Discard
	if o.verbose {
		logs = os.Stderr
	}
	cctx, stopCollector := context.WithCancel(ctx)
	collectorDone := make(chan error, 1)
	go func() {
		collectorDone <- collector.New(target, st, collector.Config{
			TargetName:      name,
			Interval:        o.interval,
			CatalogInterval: time.Hour,
			TextInterval:    o.interval,
		}, slog.New(slog.NewTextHandler(logs, nil))).Run(cctx)
	}()

	wctx, stopWorkload := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workload(wctx, app, q, o.rate, rand.New(rand.NewPCG(uint64(i), uint64(time.Now().UnixNano()))))
		}()
	}

	restore := func() {
		for _, stmt := range sc.restore {
			target.Exec(context.WithoutCancel(ctx), stmt)
		}
		if sc.reconnect {
			app.Reset()
		}
	}
	defer restore()

	sleep(ctx, o.baseline)
	for _, stmt := range sc.inject {
		if _, err := target.Exec(ctx, stmt); err != nil {
			stopWorkload()
			stopCollector()
			return outcome{}, fmt.Errorf("injecting: %w", err)
		}
	}
	if sc.reconnect {
		app.Reset()
	}
	sleep(ctx, o.fault)
	// One more poll so the last stretch of the fault is recorded.
	sleep(ctx, o.interval+time.Second)
	stopWorkload()
	wg.Wait()
	stopCollector()
	if err := <-collectorDone; err != nil {
		return outcome{}, fmt.Errorf("collector: %w", err)
	}
	if ctx.Err() != nil {
		return outcome{}, ctx.Err()
	}
	return evaluate(ctx, o, sc, st, name)
}

func workload(ctx context.Context, pool *pgxpool.Pool, q query, rate int, r *rand.Rand) {
	tick := time.NewTicker(time.Second / time.Duration(rate))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := pool.Exec(ctx, q.sql, q.arg(r)); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "  %s: %v\n", q.name, err)
			}
		}
	}
}

func evaluate(ctx context.Context, o options, sc scenario, st *store.Store, targetName string) (outcome, error) {
	targets, err := st.Targets(ctx)
	if err != nil {
		return outcome{}, err
	}
	targetID := targets[targetName]
	now, err := st.LatestSnapshot(ctx, targetID)
	if err != nil {
		return outcome{}, err
	}
	cfg := detect.DefaultConfig()
	// The window starts two polls after the fault so it doesn't straddle it.
	cfg.Window = o.fault - o.interval
	cfg.Trailing = o.baseline
	cfg.Weeks = 0

	series, err := st.Series(ctx, targetID, cfg, now)
	if err != nil {
		return outcome{}, err
	}
	found, _ := detect.Scan(series, now, cfg)
	ids := make([]int64, len(found))
	for i, f := range found {
		ids[i] = f.QueryID
	}
	info, err := st.QueryInfo(ctx, ids)
	if err != nil {
		return outcome{}, err
	}
	res := outcome{scenario: sc.name, expect: sc.expect, flagged: map[string]detect.Finding{}}
	for _, f := range found {
		res.flagged[workloadName(info[f.QueryID].Text)] = f.Finding
	}
	return res, nil
}

func workloadName(text string) string {
	if _, rest, ok := strings.Cut(text, "harness:"); ok {
		if name, _, ok := strings.Cut(rest, " "); ok {
			return name
		}
	}
	return "other: " + strings.Join(strings.Fields(text), " ")
}

func report(w io.Writer, results []outcome) (failed bool) {
	var tp, fp, fn, kindOK int
	fmt.Fprintln(w, "| scenario | query | expected | flagged | ratio | result |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|")
	for _, r := range results {
		names := map[string]bool{}
		for n := range r.expect {
			names[n] = true
		}
		for n := range r.flagged {
			names[n] = true
		}
		if len(names) == 0 {
			fmt.Fprintf(w, "| %s | (none) | – | – | | ok |\n", r.scenario)
			continue
		}
		for _, n := range slices.Sorted(maps.Keys(names)) {
			want, expected := r.expect[n]
			got, flagged := r.flagged[n]
			row := func(res string) {
				exp, fl, ratio := "–", "–", ""
				if expected {
					exp = string(want)
				}
				if flagged {
					fl, ratio = string(got.Kind), fmt.Sprintf("%.1fx", got.Ratio)
				}
				fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s |\n", r.scenario, n, exp, fl, ratio, res)
			}
			switch {
			case expected && flagged:
				tp++
				if got.Kind == want {
					kindOK++
					row("ok")
				} else {
					row("wrong kind")
				}
			case expected:
				fn++
				row("MISSED")
			default:
				fp++
				row("FALSE ALARM")
			}
		}
	}
	fmt.Fprintf(w, "\nprecision %s, recall %s, kind accuracy %s\n",
		pct(tp, tp+fp), pct(tp, tp+fn), pct(kindOK, tp))
	return fp > 0 || fn > 0
}

func pct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", a, b, 100*float64(a)/float64(b))
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
