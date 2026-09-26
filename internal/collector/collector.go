package collector

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BloodShotAK/pgblame/internal/catalog"
	"github.com/BloodShotAK/pgblame/internal/pgss"
	"github.com/BloodShotAK/pgblame/internal/store"
)

type Config struct {
	TargetName      string
	Interval        time.Duration
	CatalogInterval time.Duration
	IncludeSelf     bool
}

type Collector struct {
	target *pgxpool.Pool
	store  *store.Store
	cfg    Config
	log    *slog.Logger

	targetID int64
	readOpts pgss.ReadOptions
	queryIDs map[pgss.Key]int64
	// in memory only, so the first interval after a restart is lost
	prev *pgss.Snapshot

	warnedColumns bool
}

func New(target *pgxpool.Pool, st *store.Store, cfg Config, log *slog.Logger) *Collector {
	return &Collector{target: target, store: st, cfg: cfg, log: log}
}

func (c *Collector) Run(ctx context.Context) error {
	if err := c.init(ctx); err != nil {
		return err
	}
	c.collectCatalog(ctx)
	c.collectStatements(ctx)

	stmts := time.NewTicker(c.cfg.Interval)
	defer stmts.Stop()
	cat := time.NewTicker(c.cfg.CatalogInterval)
	defer cat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stmts.C:
			c.collectStatements(ctx)
		case <-cat.C:
			c.collectCatalog(ctx)
		}
	}
}

func (c *Collector) init(ctx context.Context) error {
	var err error
	if c.targetID, err = c.store.EnsureTarget(ctx, c.cfg.TargetName); err != nil {
		return fmt.Errorf("registering target: %w", err)
	}
	if c.queryIDs, err = c.store.QueryIDs(ctx, c.targetID); err != nil {
		return fmt.Errorf("loading known queries: %w", err)
	}
	if !c.cfg.IncludeSelf {
		if c.readOpts.ExcludeUserID, err = pgss.SelfOID(ctx, c.target); err != nil {
			return fmt.Errorf("looking up collector role: %w", err)
		}
	}
	c.log.Info("collector started", "target", c.cfg.TargetName, "target_id", c.targetID,
		"known_queries", len(c.queryIDs), "interval", c.cfg.Interval, "catalog_interval", c.cfg.CatalogInterval)
	return nil
}

func (c *Collector) collectStatements(ctx context.Context) {
	if err := c.statements(ctx); err != nil && ctx.Err() == nil {
		c.log.Error("statement collection failed", "err", err)
	}
}

func (c *Collector) statements(ctx context.Context) error {
	snap, err := pgss.Read(ctx, c.target, c.readOpts)
	if err != nil {
		return err
	}
	if c.prev == nil {
		if err := c.store.WriteSnapshot(ctx, c.targetID, snap, nil, nil); err != nil {
			return err
		}
		c.prev = snap
		c.log.Info("baseline snapshot taken", "entries", len(snap.Entries), "server_version", snap.ServerVersionNum)
		return nil
	}

	iv := pgss.Diff(c.prev, snap)
	if err := c.recordNewQueries(ctx, snap, iv); err != nil {
		return err
	}
	// Keep prev on failure; the next diff then spans both intervals.
	if err := c.store.WriteSnapshot(ctx, c.targetID, snap, &iv, c.queryIDs); err != nil {
		return err
	}
	c.prev = snap

	attrs := []any{"samples", len(iv.Samples), "entries", len(snap.Entries), "window", iv.End.Sub(iv.Start).Round(time.Second)}
	switch {
	case iv.GlobalReset:
		c.log.Warn("pg_stat_statements was reset during the interval; samples are partial", attrs...)
	case iv.EvictionSuspect:
		c.log.Warn("entries were evicted; some deltas may be understated, consider raising pg_stat_statements.max",
			append(attrs, "dealloc_delta", iv.DeallocDelta)...)
	default:
		c.log.Info("interval recorded", append(attrs, "restarted", iv.Restarted, "evicted", iv.Evicted)...)
	}
	return nil
}

func (c *Collector) recordNewQueries(ctx context.Context, snap *pgss.Snapshot, iv pgss.Interval) error {
	var unknown []pgss.Key
	for _, s := range iv.Samples {
		if _, ok := c.queryIDs[s.Key]; !ok {
			unknown = append(unknown, s.Key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	texts, err := pgss.Texts(ctx, c.target, unknown)
	if err != nil {
		return err
	}
	qs := make([]store.NewQuery, 0, len(unknown))
	for _, k := range unknown {
		q := store.NewQuery{Entry: snap.Entries[k]}
		if t, ok := texts[k]; ok {
			q.Text = &t
		}
		qs = append(qs, q)
	}
	ids, err := c.store.AddQueries(ctx, c.targetID, qs, snap.TakenAt)
	if err != nil {
		return err
	}
	for k, id := range ids {
		c.queryIDs[k] = id
	}
	return nil
}

func (c *Collector) collectCatalog(ctx context.Context) {
	if err := c.catalog(ctx); err != nil && ctx.Err() == nil {
		c.log.Error("catalog collection failed", "err", err)
	}
}

func (c *Collector) catalog(ctx context.Context) error {
	snap, err := catalog.Read(ctx, c.target)
	if err != nil {
		return err
	}
	ch, err := c.store.ApplyCatalog(ctx, c.targetID, snap)
	if err != nil {
		return err
	}
	if !snap.ColumnsComplete && !c.warnedColumns {
		c.log.Warn("pgblame.column_stats() is not installed; column statistics cover only columns this role can SELECT",
			"database", snap.DBName)
		c.warnedColumns = true
	}
	if ch.Baseline {
		c.log.Info("catalog baseline recorded", "database", snap.DBName,
			"indexes", len(snap.Indexes), "settings", len(snap.Settings), "columns", len(snap.Columns), "tables", len(snap.Tables))
		return nil
	}
	for _, x := range ch.Indexes {
		v := x.New
		if x.Kind == catalog.Removed {
			v = x.Old
		}
		c.log.Info("index "+string(x.Kind), "database", snap.DBName,
			"index", x.Key.Schema+"."+x.Key.Index, "table", x.Key.Table,
			"valid", v.Valid, "definition", v.Definition)
	}
	for _, x := range ch.Settings {
		c.log.Info("setting "+string(x.Kind), "database", snap.DBName, "name", x.Key, "old", x.Old.Value, "new", x.New.Value)
	}
	for _, x := range ch.Columns {
		level := slog.LevelDebug
		if x.Kind == catalog.Modified {
			level = slog.LevelInfo
		}
		c.log.Log(ctx, level, "column statistics "+string(x.Kind), "database", snap.DBName,
			"column", x.Key.Schema+"."+x.Key.Table+"."+x.Key.Column,
			"n_distinct", fmt.Sprintf("%g -> %g", x.Old.NDistinct, x.New.NDistinct),
			"null_frac", fmt.Sprintf("%.3f -> %.3f", x.Old.NullFrac, x.New.NullFrac))
	}
	return nil
}
