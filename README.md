# pgblame

**`git blame` for slow Postgres queries.**

pgblame detects query regressions and tells you what to blame: not just
*that* a query got slower, but *why the planner changed its mind*. That
might be a dropped index, a setting change, or a column whose distribution
shifted.

It works with any Postgres 14+, self-hosted or managed. It only needs
`pg_stat_statements` and a role with `pg_monitor`, not superuser.

> **Status: M0, collection.** pgblame records per-interval query activity
> and a versioned history of the catalog. Regression scoring, plan capture
> and explanations come next; see the [roadmap](#roadmap).

## Quickstart

This starts a Postgres under `pgbench` load, a store database, and the
collector:

```sh
docker compose up -d --build
make check   # is the monitored server set up correctly?
make logs    # watch the collector
make top     # busiest queries recorded in the last 15 minutes
```

Try breaking something and watch the collector notice:

```sh
docker compose exec target psql -U postgres -d app \
  -c "ALTER SYSTEM SET random_page_cost = 1.1" -c "SELECT pg_reload_conf()"
make logs    # level=INFO msg="setting modified" name=random_page_cost old=4 new=1.1
```

## Monitoring your own Postgres

1. **Load `pg_stat_statements`.** Add it to `shared_preload_libraries` and
   restart. On managed Postgres this goes through the provider's parameter
   settings (for example an RDS parameter group).
2. **Create the extension and a monitoring role.** Run
   [`deploy/target/setup.sql`](deploy/target/setup.sql) with your own
   password.
3. **Optional: install the column-statistics helper** in each monitored
   database: [`deploy/target/helper.sql`](deploy/target/helper.sql). Without
   it, `pg_stats` only shows columns the role can `SELECT`. The helper
   exposes planner statistics only (`null_frac`, `n_distinct`,
   `correlation`), never the histograms or most-common values that contain
   real data.
4. **Check, then collect:**

```sh
pgblame check   --target-dsn "postgres://pgblame:...@host:5432/app"
pgblame collect --target-dsn "postgres://pgblame:...@host:5432/app" \
                  --store-dsn  "postgres://...@store-host:5432/pgblame"
```

`check` reports every problem at once, each with a fix:

```
[ok]    server version                170011
[ok]    pg_stat_statements extension  version 1.11
[warn]  pg_stat_statements capacity   1203 entries evicted since the last reset (max = 5000)
          fix: raise pg_stat_statements.max so entries stop being evicted between polls
[FAIL]  privileges                    role cannot see other roles' statements
          fix: GRANT pg_monitor TO <this role>;
```

pgblame writes to its own store database, so the monitored server gets no
pgblame tables. The store migrates itself on startup.

## How collection works

### Statements

`pg_stat_statements` counters are cumulative, so pgblame snapshots the view
every interval (default 1m) and stores the difference. It stores raw deltas
rather than means, so that coarser rollups can simply sum them. Query texts
are fetched only for entries seen for the first time, because reading the
extension's text file on every poll is expensive.

Counting correctly depends on handling the cases where counters restart:

| Situation | How it's detected | What's recorded |
|---|---|---|
| New entry since the last poll | Key absent from the previous snapshot | Full counters. Its whole life is inside the interval, so nothing is lost |
| `pg_stat_statements_reset()` | `stats_reset` moved, or `dealloc` went backwards | Every sample marked `partial` |
| Entry evicted and recreated (PG17+) | `stats_since` changed | Sample marked `partial` |
| Entry evicted and recreated (PG14–16) | Some counter went down | Sample marked `partial` |
| Evicted, recreated, and already past its old counts (PG14–16) | Can't be detected per entry | Interval marked `eviction_suspect` |
| Server crash, which loses the stats file | `dealloc` reset | Treated as a global reset |

A `partial` sample is missing whatever ran between the previous poll and the
reset, so its totals are a lower bound. Per-call figures remain usable.

### Catalog

Every catalog interval (default 5m), pgblame reads each monitored
database's indexes, plan-relevant settings (query tuning, memory,
autovacuum), column statistics and table sizes. Indexes, settings and column
statistics are stored as **validity ranges**: a new row only when something
changes. That makes the history cheap to keep and doubles as an event log:

- An index dropped, created, or left **invalid** by a failed
  `CREATE INDEX CONCURRENTLY`. Such an index still exists but the planner
  ignores it.
- A setting such as `random_page_cost` or `work_mem` changed, including
  per-database overrides.
- A column's statistics shifted enough to change estimates: `n_distinct`
  moved 2×, `null_frac` by 0.1, or `correlation` by 0.3. Routine `ANALYZE`
  noise is ignored.

## Development

```sh
make test               # unit tests
docker compose up -d target
make test-integration   # against a real server: exact call counts, resets, index drops
```

CI runs the integration suite against PostgreSQL 14, 16, 17 and 18.

```
cmd/pgblame        CLI: check, collect, top
internal/pgss        pg_stat_statements snapshots and delta logic
internal/catalog     catalog snapshots and change detection
internal/store       store schema, migrations, writes
internal/collector   polling loop
internal/preflight   server readiness checks
```

## Roadmap

- **M1: Regression scoring.** Hour-of-week baselines, median/MAD scoring,
  ranking by extra database time, and time-per-row to separate data growth
  from plan changes. Plus a fault-injection harness to measure precision and
  recall.
- **M2: Plan capture.** `EXPLAIN (GENERIC_PLAN)` on PG16+, `auto_explain`
  log adapters for self-hosted and managed servers, and structural plan
  fingerprints.
- **M3: Explanations.** Plan tree diffs joined with catalog history, for
  example "Index Scan → Seq Scan; `orders_customer_idx` was dropped 4 minutes
  earlier". Optional `hypopg` confirmation.
- **M4: Deploys.** A deploy webhook, correlation with regressions, and a web UI.
- **M5: MCP server.** Lets an assistant answer "why did checkout get slow?"
  from recorded evidence.
