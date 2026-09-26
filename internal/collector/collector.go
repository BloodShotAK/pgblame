package collector

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BloodShotAK/pgblame/internal/catalog"
	"github.com/BloodShotAK/pgblame/internal/detect"
	"github.com/BloodShotAK/pgblame/internal/pgss"
	"github.com/BloodShotAK/pgblame/internal/store"
)

type Config struct {
	TargetName      string
	Interval        time.Duration
	CatalogInterval time.Duration
	// Zero disables detection.
	TextInterval   time.Duration
	DetectInterval time.Duration
	Detect         detect.Config
	Retention      store.Retention
	IncludeSelf    bool
}

type Collector struct {
	target *pgxpool.Pool
	store  *store.Store
	cfg    Config
	log    *slog.Logger

	targetID int64
	readOpts pgss.ReadOptions
	queryIDs map[pgss.Key]int64
	// recorded queries still waiting for their text
	needText  map[pgss.Key]int64
	lastTexts time.Time
	// in memory only, so the first interval after a restart is lost
	prev *pgss.Snapshot

	analyzed        map[catalog.TableKey]time.Time
	lastFullCatalog time.Time
	warnedColumns   bool
}

func New(target *pgxpool.Pool, st *store.Store, cfg Config, log *slog.Logger) *Collector {
	if cfg.TextInterval == 0 {
		cfg.TextInterval = 5 * time.Minute
	}
	if cfg.Retention == (store.Retention{}) {
		cfg.Retention = store.DefaultRetention()
	}
	return &Collector{target: target, store: st, cfg: cfg, log: log}
}

func (c *Collector) Run(ctx context.Context) error {
	if err := c.init(ctx); err != nil {
		return err
	}
	c.maintain(ctx)
	c.collectCatalog(ctx)
	c.collectStatements(ctx)

	stmts := time.NewTicker(c.cfg.Interval)
	defer stmts.Stop()
	cat := time.NewTicker(c.cfg.CatalogInterval)
	defer cat.Stop()
	maint := time.NewTicker(time.Hour)
	defer maint.Stop()
	var detectTick <-chan time.Time
	if c.cfg.DetectInterval > 0 {
		t := time.NewTicker(c.cfg.DetectInterval)
		defer t.Stop()
		detectTick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stmts.C:
			c.collectStatements(ctx)
		case <-cat.C:
			c.collectCatalog(ctx)
		case <-maint.C:
			c.maintain(ctx)
		case <-detectTick:
			if err := c.detect(ctx); err != nil && ctx.Err() == nil {
				c.log.Error("detection failed", "err", err)
			}
		}
	}
}

func (c *Collector) init(ctx context.Context) error {
	var err error
	if c.targetID, err = c.store.EnsureTarget(ctx, c.cfg.TargetName); err != nil {
		return fmt.Errorf("registering target: %w", err)
	}
	if c.queryIDs, c.needText, err = c.store.QueryIDs(ctx, c.targetID); err != nil {
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
	if err := c.fetchTexts(ctx, snap); err != nil {
		c.log.Error("fetching query texts failed", "err", err)
	}

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
	var qs []store.NewQuery
	for _, s := range iv.Samples {
		if _, ok := c.queryIDs[s.Key]; !ok {
			qs = append(qs, store.NewQuery{Entry: snap.Entries[s.Key]})
		}
	}
	ids, err := c.store.AddQueries(ctx, c.targetID, qs, snap.TakenAt)
	if err != nil {
		return err
	}
	for k, id := range ids {
		c.queryIDs[k] = id
		c.needText[k] = id
	}
	return nil
}

// fetchTexts fills in texts for recorded queries that lack one. Reading
// texts makes the server load the extension's whole query-text file, however
// few are asked for, so it happens at most once per TextInterval.
func (c *Collector) fetchTexts(ctx context.Context, snap *pgss.Snapshot) error {
	if len(c.needText) == 0 || time.Since(c.lastTexts) < c.cfg.TextInterval {
		return nil
	}
	c.lastTexts = time.Now()
	var keys []pgss.Key
	for k := range c.needText {
		if _, ok := snap.Entries[k]; ok {
			keys = append(keys, k)
		} else {
			// Evicted, so its text is gone for good.
			delete(c.needText, k)
		}
	}
	texts, err := pgss.Texts(ctx, c.target, keys)
	if err != nil {
		return err
	}
	byID := make(map[int64]string, len(texts))
	for k, t := range texts {
		byID[c.needText[k]] = t
		delete(c.needText, k)
	}
	return c.store.SetQueryTexts(ctx, byID)
}

func (c *Collector) collectCatalog(ctx context.Context) {
	if err := c.catalog(ctx); err != nil && ctx.Err() == nil {
		c.log.Error("catalog collection failed", "err", err)
	}
}

func (c *Collector) catalog(ctx context.Context) error {
	// A fresh session, because ALTER DATABASE ... SET only applies at connection start.
	conn, err := pgx.ConnectConfig(ctx, c.target.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	// Statistics can also change without ANALYZE (a restore, say), so
	// everything is re-read once a day.
	if time.Since(c.lastFullCatalog) > 24*time.Hour {
		c.analyzed = nil
	}
	snap, err := catalog.Read(ctx, conn, c.analyzed)
	if err != nil {
		return err
	}
	ch, err := c.store.ApplyCatalog(ctx, c.targetID, snap)
	if err != nil {
		return err
	}
	if c.analyzed == nil {
		c.lastFullCatalog = time.Now()
	}
	c.analyzed = make(map[catalog.TableKey]time.Time, len(snap.Tables))
	for _, t := range snap.Tables {
		c.analyzed[t.TableKey] = t.AnalyzedAt
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
	for _, x := range ch.Tables {
		c.log.Info("table size "+string(x.Kind), "database", snap.DBName, "table", x.Key.Schema+"."+x.Key.Table,
			"rows", fmt.Sprintf("%d -> %d", x.Old.RelTuples, x.New.RelTuples),
			"pages", fmt.Sprintf("%d -> %d", x.Old.RelPages, x.New.RelPages))
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

func (c *Collector) detect(ctx context.Context) error {
	if c.prev == nil {
		return nil
	}
	// The last snapshot's server timestamp, so the window lines up with
	// recorded intervals regardless of clock skew.
	now := c.prev.TakenAt
	series, err := c.store.Series(ctx, c.targetID, c.cfg.Detect, now)
	if err != nil {
		return err
	}
	found, healthy := detect.Scan(series, now, c.cfg.Detect)
	opened, closed, err := c.store.ApplyFindings(ctx, now, found, healthy)
	if err != nil || len(opened)+len(closed) == 0 {
		return err
	}
	info, err := c.store.QueryInfo(ctx, append(slices.Clone(opened), closed...))
	if err != nil {
		return err
	}
	for _, f := range found {
		if !slices.Contains(opened, f.QueryID) {
			continue
		}
		q := info[f.QueryID]
		c.log.Warn("regression opened", "kind", f.Kind, "database", q.DBName, "queryid", q.QueryID,
			"baseline_ms", round(f.BaselineMS), "current_ms", round(f.CurrentMS), "ratio", round(f.Ratio),
			"extra_per_hour", f.ExtraPerHour.Round(time.Millisecond), "baseline", f.Baseline, "query", shorten(q.Text, 120))
	}
	for _, id := range closed {
		q := info[id]
		c.log.Info("regression closed", "database", q.DBName, "queryid", q.QueryID, "query", shorten(q.Text, 120))
	}
	return nil
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

func shorten(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func (c *Collector) maintain(ctx context.Context) {
	res, err := c.store.Maintain(ctx, time.Now(), c.cfg.Retention)
	switch {
	case err != nil && ctx.Err() == nil:
		c.log.Error("store maintenance failed", "err", err)
	case err == nil && !res.Skipped:
		c.log.Debug("store maintenance done", "rolled_up", res.RolledUp, "dropped_days", res.DroppedDays,
			"deleted_hourly", res.DeletedHourly, "deleted_snapshots", res.DeletedSnapshot)
	}
}
