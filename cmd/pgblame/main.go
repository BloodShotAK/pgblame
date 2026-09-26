// Command pgblame detects Postgres query regressions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BloodShotAK/pgblame/internal/collector"
	"github.com/BloodShotAK/pgblame/internal/preflight"
	"github.com/BloodShotAK/pgblame/internal/store"
)

var version = "dev"

const usage = `pgblame detects Postgres query regressions.

Usage:
  pgblame check   --target-dsn DSN    verify a server is ready to be monitored
  pgblame collect --target-dsn DSN --store-dsn DSN
                                        poll the server and record what it runs
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
