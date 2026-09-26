# pgblame

[![CI](https://github.com/BloodShotAK/pgblame/actions/workflows/ci.yml/badge.svg)](https://github.com/BloodShotAK/pgblame/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/BloodShotAK/pgblame)](go.mod)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14%E2%80%9318-336791.svg)](#monitoring-your-own-postgres)

**`git blame` for slow Postgres queries.**

pgblame watches your Postgres, notices when a query gets slower than it
normally is, and tells you what changed: a dropped index, a planner setting,
a column whose statistics shifted.

```
$ pgblame regressions --target local
as of 2026-09-26 18:07:33: 1 regressed, 6 healthy, 2 with too little traffic to judge

  extra/hour  baseline ms  now ms  ratio  calls       kind  query
     13.919s        0.017   0.206  12.2x   2759  more_work  SELECT abalance FROM pgbench_accounts WHERE aid = $1
```

## Why

A query that ran in 2ms yesterday runs in 200ms today, and usually nothing
in the application changed. The database did. A migration dropped an index,
a `CREATE INDEX CONCURRENTLY` failed and left an invalid one behind,
autovacuum ran `ANALYZE` and the planner's row estimates moved, or someone
changed a setting in a parameter group. The planner chose a different plan
and nobody was told.

Postgres already has the data to catch this. `pg_stat_statements` counts
every query, and the catalog knows every index and setting. But
`pg_stat_statements` only holds running totals since its last reset, and the
catalog only shows the present. Neither can tell you "this query got slower
at 14:05, and at 14:03 this index disappeared." So users notice regressions
first, and someone finds the cause by hand, hours later.

## What pgblame does

1. **Records.** Every minute it snapshots `pg_stat_statements` and stores
   what each query did in that minute. Every five minutes it records what
   changed in the catalog: indexes, planner settings, column statistics and
   table sizes.
2. **Detects.** Every minute it compares each query's recent latency with
   that query's own history (the same hour in previous weeks, or the last
   day). It opens a regression when a query is clearly and expensively
   slower, and closes it when the query recovers.
3. **Blames.** It classifies each regression by how the work per call
   changed. "Same rows, but 50× the blocks read" points at a plan change,
   not at more data. It also keeps the catalog history that the next
   milestones use to name the exact cause.

## Features

- **Regression detection** against per-query baselines that account for
  time of day and day of week, using robust statistics that noisy minutes
  can't skew, ranked by the extra database time each regression costs.
- **A first diagnosis** for every regression: plan change, more rows, cache
  misses, or contention.
- **Catalog history** of indexes (including invalid ones), planner settings,
  column statistics and table sizes, recorded only when they change.
- **Correct counting** through `pg_stat_statements` resets, evictions and
  server crashes.
- **A preflight check** that lists everything missing on your server, each
  with the fix.
- **No provider lock-in.** It needs only plain SQL, `pg_stat_statements` and
  the built-in `pg_monitor` role, so it works the same on self-hosted and
  managed Postgres 14+.
- **Safe for production.** It is read-only, uses at most two connections,
  enforces strict timeouts, and costs about 0.08% of one CPU core.
- **Your data stays yours.** You host the store. Query texts are normalized
  by Postgres, and pgblame redacts passwords before a text leaves your
  server.
- **A single static binary.** The Dockerfile builds a distroless, non-root
  image.

## Quickstart

This starts a Postgres under `pgbench` load, a store database, and the
collector:

```sh
docker compose up -d --build
make check   # is the monitored server set up correctly?
make logs    # watch the collector
```

Break something and watch it get blamed:

```sh
docker compose exec target psql -U postgres -d app \
  -c "ALTER SYSTEM SET enable_indexscan = off" \
  -c "ALTER SYSTEM SET enable_bitmapscan = off" \
  -c "SELECT pg_reload_conf()"
make logs          # msg="setting modified" name=enable_indexscan old=on new=off
                   # msg="regression opened" kind=more_work ratio=... query="UPDATE pgbench_accounts ..."
make regressions   # ranked list of what got slower
```

Put it back with `ALTER SYSTEM RESET ALL` and the regression closes itself.

## Architecture

```mermaid
flowchart LR
    db[("Your Postgres<br/>pg_stat_statements<br/>and catalog")]
    subgraph collect["pgblame collect"]
        direction TB
        poll["Poll statements<br/>every minute"]
        snap["Snapshot catalog<br/>every 5 minutes"]
        detect["Detect regressions<br/>every minute"]
    end
    store[("Store<br/>samples, rollups,<br/>catalog history,<br/>regressions")]
    out["pgblame regressions<br/>and logs"]

    db -- "read-only" --> poll
    db -- "read-only" --> snap
    poll --> store
    snap --> store
    store <--> detect
    detect --> out
```

`pgblame collect` is one process. It only reads the database it watches,
over at most two connections, and writes everything to a store database you
host, which can be a small Postgres anywhere. Inside the store, per-minute
samples live in daily partitions for 3 days, and an hourly maintenance pass
rolls them up into hourly totals kept for 35 days. `pgblame regressions`, `top` and `check` are
short-lived commands that read the store or run preflight checks.

The code is split along the same lines. `internal/pgss` and
`internal/catalog` are the only Postgres-specific readers. `internal/detect`
works on plain per-interval counters, so another database engine would need
new readers but not new detection logic.

## Resource usage

pgblame is a guest on your database and behaves like one:

- **Read-only sessions.** At most two connections, each with
  `default_transaction_read_only`, a 10s `statement_timeout` and a 1s
  `lock_timeout`. It gives up rather than wait on your workload.
- **Cheap reads.** Query texts are fetched at most every 5 minutes, since
  reading them makes Postgres load the whole query-text file. Column
  statistics are re-read only for tables `ANALYZE` touched since the last
  cycle, one indexed lookup per table in chunks of 250.
- **No growth on the target.** Nothing is written there.

Measured with [`cmd/overhead`](cmd/overhead) on a database with 2,000
tables, 4,000 indexes, 20,000 columns and 4,000 distinct queries. These are
client-side wall times, an upper bound on server time:

| Read | How often | Time |
|---|---|---|
| `pg_stat_statements` snapshot | every minute | 26ms |
| query texts | at most every 5 minutes | 25ms |
| catalog, nothing re-analyzed | every 5 minutes | 79ms |
| catalog, 20 tables re-analyzed | every 5 minutes | 75ms |
| catalog, full | at startup and daily | 272ms |

That comes to about 46ms of database time a minute, around 0.08% of one
core.

**The store stays bounded too.** Per-interval samples live in daily
partitions kept for 3 days and dropped whole, which avoids `DELETE` bloat.
Hourly rollups are kept for 35 days to feed the four-week baseline. Catalog
history is stored only when something changes. Both retention periods are
flags on `collect`.

## How it compares

| | Reading `pg_stat_statements` by hand | Metrics dashboards (for example postgres_exporter with Grafana) | pgblame |
|---|---|---|---|
| History per query | No, only totals since the last reset | Possible, though per-query metrics are high-cardinality | Yes, per minute, then hourly |
| Knows what's normal for each query | No | You write the alert thresholds | Learns it per query, by hour of the week |
| Records index, setting and statistics changes | No | Not by itself | Yes, as a queryable history |
| Where your data goes | Stays put | Your metrics stack | Your own store database |

Hosted monitoring services cover much of this and more. They're the right
choice if you want a managed product. pgblame is for teams that want a small
open-source tool, focused on answering "what got slower, and why", whose
data never leaves their own infrastructure.

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
   exposes planner statistics only, never data; [SECURITY.md](SECURITY.md)
   has the details.
4. **Check, then collect:**

```sh
pgblame check   --target-dsn "postgres://pgblame:...@host:5432/app?sslmode=verify-full"
pgblame collect --target-dsn "postgres://pgblame:...@host:5432/app?sslmode=verify-full" \
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
[warn]  encryption                    connection is not encrypted; query texts and credentials cross the network in clear
          fix: add sslmode=verify-full (with sslrootcert) to the DSN
```

pgblame writes to its own store database, so the monitored server gets no
pgblame tables. The store migrates itself on startup.

## Detection

Every minute the collector compares each query's recent latency (the last 15
minutes) with a baseline and keeps one open regression per query until it
recovers.

- **Baseline.** pgblame uses the same hour in each of the previous four
  weeks, so a nightly batch that is always slow at 2am isn't flagged. With
  less history, it falls back to the trailing 24 hours. Time inside a past
  or ongoing regression is left out, so a long regression never becomes its
  own baseline.
- **Robust scoring.** The baseline is the median of per-interval latencies,
  and the spread is the median absolute deviation. A few bad intervals can't
  move either one.
- **Gates.** A query is flagged only if it is at least 1.5× slower, at least
  4 robust standard deviations out, and costing at least a second of extra
  database time per hour. Queries with fewer than 30 calls aren't judged.
- **Ranking.** Findings are ordered by extra database time, so a 1ms query
  that became 3ms at a million calls an hour outranks a report that went
  from 1s to 5s once.

Each regression gets a first guess at its cause, from how the work per call
changed:

| Kind | Signal | Usually means |
|---|---|---|
| `more_work` | Same rows per call, 2×+ the blocks touched | The plan changed: a dropped index, a setting, a statistics shift |
| `more_rows` | Rows per call grew about as much as latency | The data or the parameters changed, not the plan |
| `cache_misses` | Share of blocks read from disk jumped | The working set no longer fits in memory |
| `slower_same_work` | None of the above | Contention, locks or I/O |

### Accuracy

[`cmd/harness`](cmd/harness) runs a steady workload, injects real faults,
and scores what pgblame reports: a control run with no fault, a dropped
index, index scans disabled by `ALTER DATABASE`, and data growth. CI runs it
on every push. Results from the latest local run:

| Scenario | Expected | Flagged | Slowdown |
|---|---|---|---|
| control | nothing | nothing | |
| drop_index | orders_by_customer `more_work` | `more_work` | 124× |
| planner_setting | 4 queries `more_work` | all 4 `more_work` | 28–139× |
| data_growth | orders_by_customer `more_rows` | `more_rows` | 4.4× |

Both local runs so far scored precision 6/6, recall 6/6, cause 6/6. Four
scenarios is a small sample, which is why the harness runs on every CI push.

## How collection works

`pg_stat_statements` counters are cumulative, so pgblame snapshots the view
every interval and stores the difference as raw deltas, which rollups can
sum. Counting correctly depends on handling the cases where counters
restart:

| Situation | How it's detected | What's recorded |
|---|---|---|
| New entry since the last poll | Key absent from the previous snapshot | Full counters. Its whole life is inside the interval |
| `pg_stat_statements_reset()` | `stats_reset` moved, or `dealloc` went backwards | Every sample marked `partial` |
| Entry evicted and recreated (PG17+) | `stats_since` changed | Sample marked `partial` |
| Entry evicted and recreated (PG14–16) | Some counter went down | Sample marked `partial` |
| Evicted, recreated, and already past its old counts (PG14–16) | Can't be detected per entry | Interval marked `eviction_suspect` and left out of detection |
| Server crash, which loses the stats file | `dealloc` reset | Treated as a global reset |

The catalog is stored as **validity ranges**, a new row only when something
changes, which doubles as an event log for explanations:

- An index created, dropped, or left **invalid** by a failed
  `CREATE INDEX CONCURRENTLY`. An invalid index still exists but the
  planner ignores it.
- A planner, memory or autovacuum setting changed, including
  `ALTER DATABASE ... SET` overrides.
- A column's statistics shifted enough to change estimates: `n_distinct`
  moved 2×, `null_frac` by 0.1, or `correlation` by 0.3.
- A table's size moved by 20% or more.

## Security

pgblame needs only `pg_monitor` and never writes to the database it
watches. [SECURITY.md](SECURITY.md) explains what it can access, how the
optional helper function is locked down, how query texts are redacted, and
how to report a vulnerability.

## Development

```sh
make test               # unit tests
docker compose up -d target store
make test-integration   # against a real server: exact call counts, resets, index drops, retention
make harness            # detection accuracy against injected faults (~7 minutes)
make overhead           # read costs on a 2,000-table database
```

CI runs the integration suite against PostgreSQL 14, 16, 17 and 18, plus
`govulncheck` and the harness.

```
cmd/pgblame          CLI: check, collect, regressions, top
cmd/harness          fault injection and accuracy scoring
cmd/overhead         read-cost measurement
internal/pgss        pg_stat_statements snapshots, deltas, redaction
internal/catalog     catalog snapshots and change detection
internal/detect      baselines, scoring and classification
internal/store       store schema, migrations, retention
internal/collector   polling loop
internal/preflight   server readiness and safety checks
```

## Roadmap

- ~~**M0: Collection.**~~
- ~~**M1: Detection.**~~
- **M2: Plan capture.** `EXPLAIN (GENERIC_PLAN)` on PG16+, `auto_explain`
  log adapters for self-hosted and managed servers, and structural plan
  fingerprints.
- **M3: Explanations.** Plan tree diffs joined with catalog history, for
  example "Index Scan → Seq Scan; `orders_customer_idx` was dropped 4 minutes
  earlier". Optional `hypopg` confirmation.
- **M4: Deploys.** A deploy webhook, correlation with regressions, and a web UI.
- **M5: MCP server.** Lets an assistant answer "why did checkout get slow?"
  from recorded evidence.

## License

[Apache License 2.0](LICENSE).
