# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/BloodShotAK/pgculprit/security/advisories/new),
not in a public issue. You'll get a response within a week.

## What pgculprit can touch

**The monitored server** is only read from. Every session pgculprit opens sets
`default_transaction_read_only = on`, `statement_timeout = 10s` and
`lock_timeout = 1s`, and it holds at most two connections. It needs the
built-in `pg_monitor` role and nothing more; `pgculprit check` warns if it is
connected as a superuser.

**The optional `pgculprit.column_stats()` helper** is a `SECURITY DEFINER`
function, so it deserves scrutiny. It has a pinned `search_path`, runs no
dynamic SQL, can be executed only by the `pgculprit` role, and returns only
`null_frac`, `n_distinct` and `correlation`. It never returns
`most_common_vals` or `histogram_bounds`, which contain real data values.

**The store** holds normalized query texts, where `pg_stat_statements` has
already replaced literals with `$1`, `$2` and so on. Some utility statements
are not normalized, so pgculprit redacts passwords (`CREATE ROLE ... PASSWORD`,
user mapping options, `password=` in connection strings) before a text leaves
the monitored server. Even so, query texts describe your schema and access
patterns, so treat the store as sensitive.

## Hardening

- Connect with `sslmode=verify-full`; `pgculprit check` warns about unencrypted
  connections to remote servers.
- Pass credentials through `PGCULPRIT_*_DSN`, `PGPASSWORD` or a `.pgpass` file
  rather than command-line flags, which other local users can see in `ps`.
- Restrict access to the store database to the collector and the people who
  read it.
- The container image is distroless and runs as a non-root user.
