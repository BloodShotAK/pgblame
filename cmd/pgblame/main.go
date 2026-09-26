// Command pgblame detects Postgres query regressions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BloodShotAK/pgblame/internal/collector"
	"github.com/BloodShotAK/pgblame/internal/detect"
	"github.com/BloodShotAK/pgblame/internal/preflight"
	"github.com/BloodShotAK/pgblame/internal/store"
)

var version = "dev"

const usage = `pgblame detects Postgres query regressions.

Usage:
  pgblame check   --target-dsn DSN    verify a server is ready to be monitored
  pgblame collect --target-dsn DSN --store-dsn DSN
                                        poll the server and record what it runs
  pgblame regressions --store-dsn DSN show queries that got slower than their baseline
  pgblame top     --store-dsn DSN     show the busiest recorded queries
  pgblame version

DSNs can also come from PGBLAME_TARGET_DSN and PGBLAME_STORE_DSN.
Run "pgblame <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "check":
		err = runCheck(ctx, args)
	case "collect":
		err = runCollect(ctx, args)
	case "regressions":
		err = runRegressions(ctx, args)
	case "top":
		err = runTop(ctx, args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgblame:", err)
		os.Exit(1)
	}
}

func dsnFlag(fs *flag.FlagSet, name, env, help string) *string {
	return fs.String(name, os.Getenv(env), help+" (env "+env+")")
}

func connect(ctx context.Context, what, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, fmt.Errorf("no %s DSN given", what)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing %s DSN: %w", what, err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "pgblame"
	if what == "target" {
		// pgblame is a guest on the monitored server: at most two sessions,
		// read-only, and it gives up rather than wait on locks or run long.
		cfg.MaxConns = 2
		for k, v := range map[string]string{
			"default_transaction_read_only":       "on",
			"statement_timeout":                   "10s",
			"lock_timeout":                        "1s",
			"idle_in_transaction_session_timeout": "30s",
		} {
			if _, set := cfg.ConnConfig.RuntimeParams[k]; !set {
				cfg.ConnConfig.RuntimeParams[k] = v
			}
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", what, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to %s: %w", what, err)
	}
	return pool, nil
}

func runCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	targetDSN := dsnFlag(fs, "target-dsn", "PGBLAME_TARGET_DSN", "server to monitor")
	fs.Parse(args)

	target, err := connect(ctx, "target", *targetDSN)
	if err != nil {
		return err
	}
	defer target.Close()

	failed := false
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range preflight.Run(ctx, target) {
		fmt.Fprintf(w, "[%s]\t%s\t%s\n", r.Status, r.Check, r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(w, "\t\t  fix: %s\n", r.Fix)
		}
		failed = failed || r.Status == preflight.Fail
	}
	w.Flush()
	if failed {
		return errors.New("target is not ready")
	}
	return nil
}

func runCollect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	targetDSN := dsnFlag(fs, "target-dsn", "PGBLAME_TARGET_DSN", "server to monitor")
	storeDSN := dsnFlag(fs, "store-dsn", "PGBLAME_STORE_DSN", "database pgblame writes to")
	name := fs.String("target-name", "", "stable name for the target (default host:port/database)")
	interval := fs.Duration("interval", time.Minute, "how often to poll pg_stat_statements")
	catalogInterval := fs.Duration("catalog-interval", 5*time.Minute, "how often to snapshot the catalog")
	detectInterval := fs.Duration("detect-interval", time.Minute, "how often to look for regressions (0 disables)")
	detectCfg := detectFlags(fs)
	retention := store.DefaultRetention()
	fs.DurationVar(&retention.Raw, "raw-retention", retention.Raw, "how long per-interval samples are kept")
	fs.DurationVar(&retention.Hourly, "hourly-retention", retention.Hourly, "how long hourly rollups are kept")
	includeSelf := fs.Bool("include-self", false, "keep pgblame's own queries in the data")
	logFormat := fs.String("log-format", "text", "text or json")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	log := newLogger(os.Stderr, *logFormat, *verbose)

	target, err := connect(ctx, "target", *targetDSN)
	if err != nil {
		return err
	}
	defer target.Close()
	st, err := connect(ctx, "store", *storeDSN)
	if err != nil {
		return err
	}
	defer st.Close()

	s := store.New(st)
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("migrating store: %w", err)
	}

	if *name == "" {
		c := target.Config().ConnConfig
		*name = fmt.Sprintf("%s:%d/%s", c.Host, c.Port, c.Database)
	}
	return collector.New(target, s, collector.Config{
		TargetName:      *name,
		Interval:        *interval,
		CatalogInterval: *catalogInterval,
		DetectInterval:  *detectInterval,
		Detect:          *detectCfg,
		Retention:       retention,
		IncludeSelf:     *includeSelf,
	}, log).Run(ctx)
}

func runTop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	storeDSN := dsnFlag(fs, "store-dsn", "PGBLAME_STORE_DSN", "database pgblame writes to")
	since := fs.Duration("since", 15*time.Minute, "window to aggregate over")
	limit := fs.Int("n", 10, "number of queries to show")
	fs.Parse(args)

	st, err := connect(ctx, "store", *storeDSN)
	if err != nil {
		return err
	}
	defer st.Close()

	top, err := store.New(st).Top(ctx, time.Now().Add(-*since), *limit)
	if err != nil {
		return err
	}
	if len(top) == 0 {
		fmt.Println("no complete intervals recorded in that window yet")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "calls\ttotal ms\tmean ms\trows/call\t database\t query\t")
	for _, t := range top {
		fmt.Fprintf(w, "%d\t%.0f\t%.3f\t%.1f\t %s\t %s\t\n", t.Calls, t.TotalMS, t.MeanMS, t.RowsCall, t.DBName, oneLine(t.Text, 70))
	}
	return w.Flush()
}

func detectFlags(fs *flag.FlagSet) *detect.Config {
	c := detect.DefaultConfig()
	fs.DurationVar(&c.Window, "window", c.Window, "recent period to judge")
	fs.DurationVar(&c.Trailing, "trailing", c.Trailing, "baseline period before the window, used when prior weeks lack data")
	fs.Float64Var(&c.MinRatio, "min-ratio", c.MinRatio, "minimum slowdown, as current / baseline latency")
	fs.Float64Var(&c.MinZ, "min-z", c.MinZ, "minimum robust z-score")
	fs.DurationVar(&c.MinExtraPerHour, "min-extra", c.MinExtraPerHour, "minimum extra database time per hour")
	return &c
}

func runRegressions(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("regressions", flag.ExitOnError)
	storeDSN := dsnFlag(fs, "store-dsn", "PGBLAME_STORE_DSN", "database pgblame writes to")
	targetName := fs.String("target", "", "target name (needed when the store has several)")
	cfg := detectFlags(fs)
	fs.Parse(args)

	pool, err := connect(ctx, "store", *storeDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool)

	targetID, err := pickTarget(ctx, st, *targetName)
	if err != nil {
		return err
	}
	now, err := st.LatestSnapshot(ctx, targetID)
	if err != nil {
		return err
	}
	series, err := st.Series(ctx, targetID, *cfg, now)
	if err != nil {
		return err
	}
	found, healthy := detect.Scan(series, now, *cfg)
	fmt.Printf("as of %s: %d regressed, %d healthy, %d with too little traffic to judge\n\n",
		now.Local().Format(time.DateTime), len(found), len(healthy), len(series)-len(found)-len(healthy))
	if len(found) == 0 {
		return nil
	}

	ids := make([]int64, len(found))
	for i, f := range found {
		ids[i] = f.QueryID
	}
	info, err := st.QueryInfo(ctx, ids)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "extra/hour\tbaseline ms\tnow ms\tratio\tcalls\t kind\t query\t")
	for _, f := range found {
		fmt.Fprintf(w, "%s\t%.3f\t%.3f\t%.1fx\t%d\t %s\t %s\t\n",
			f.ExtraPerHour.Round(time.Millisecond), f.BaselineMS, f.CurrentMS, f.Ratio, f.Calls, f.Kind, oneLine(info[f.QueryID].Text, 60))
	}
	return w.Flush()
}

func pickTarget(ctx context.Context, st *store.Store, name string) (int64, error) {
	targets, err := st.Targets(ctx)
	if err != nil {
		return 0, err
	}
	if name != "" {
		id, ok := targets[name]
		if !ok {
			return 0, fmt.Errorf("no target named %q", name)
		}
		return id, nil
	}
	if len(targets) == 1 {
		for _, id := range targets {
			return id, nil
		}
	}
	names := slices.Sorted(maps.Keys(targets))
	return 0, fmt.Errorf("pick a target with --target: %s", strings.Join(names, ", "))
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func newLogger(w io.Writer, format string, verbose bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if verbose {
		opts.Level = slog.LevelDebug
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
