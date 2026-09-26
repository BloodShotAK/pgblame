// Package preflight checks a server is ready for pgculprit using plain SQL,
// so it behaves the same on self-hosted and managed Postgres.
package preflight

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/BloodShotAK/pgculprit/internal/pgss"
)

type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "FAIL"
)

type Result struct {
	Check  string
	Status Status
	Detail string
	Fix    string
}

type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func Run(ctx context.Context, q Querier) []Result {
	var out []Result
	add := func(r Result) { out = append(out, r) }

	var version int
	if err := q.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		return []Result{{Check: "connect", Status: Fail, Detail: err.Error()}}
	}
	switch {
	case version < pgss.MinServerVersionNum:
		add(Result{"server version", Fail, fmt.Sprintf("%d", version), "pgculprit needs PostgreSQL 14 or newer"})
	case version < 160000:
		add(Result{"server version", OK, fmt.Sprintf("%d; plan capture via EXPLAIN (GENERIC_PLAN) needs 16+", version), ""})
	case version < 170000:
		add(Result{"server version", OK, fmt.Sprintf("%d; eviction detection is approximate before 17", version), ""})
	default:
		add(Result{"server version", OK, fmt.Sprintf("%d", version), ""})
	}

	var extVersion *string
	if err := q.QueryRow(ctx, `SELECT extversion FROM pg_extension WHERE extname = 'pg_stat_statements'`).Scan(&extVersion); err != nil && err != pgx.ErrNoRows {
		add(Result{"pg_stat_statements extension", Fail, err.Error(), ""})
	} else if extVersion == nil {
		add(Result{"pg_stat_statements extension", Fail, "not installed in this database",
			"CREATE EXTENSION pg_stat_statements; (it must also be in shared_preload_libraries: on managed Postgres that is set through the provider's parameter settings)"})
	} else {
		var dealloc int64
		var max string
		err := q.QueryRow(ctx, `SELECT dealloc, current_setting('pg_stat_statements.max') FROM pg_stat_statements_info`).Scan(&dealloc, &max)
		switch {
		case err != nil && strings.Contains(err.Error(), "shared_preload_libraries"):
			add(Result{"pg_stat_statements extension", Fail, "installed but not loaded",
				"add pg_stat_statements to shared_preload_libraries and restart the server"})
		case err != nil:
			add(Result{"pg_stat_statements extension", Fail, fmt.Sprintf("version %s: %v", *extVersion, err),
				"ALTER EXTENSION pg_stat_statements UPDATE; (pg_stat_statements_info needs extension version 1.9+)"})
		default:
			add(Result{"pg_stat_statements extension", OK, "version " + *extVersion, ""})
			if dealloc > 0 {
				add(Result{"pg_stat_statements capacity", Warn,
					fmt.Sprintf("%d entries evicted since the last reset (max = %s)", dealloc, max),
					"raise pg_stat_statements.max so entries stop being evicted between polls"})
			} else {
				add(Result{"pg_stat_statements capacity", OK, "no evictions (max = " + max + ")", ""})
			}
		}
	}

	var readAllStats bool
	if err := q.QueryRow(ctx, `SELECT pg_has_role(current_user, 'pg_read_all_stats', 'MEMBER')`).Scan(&readAllStats); err != nil {
		add(Result{"privileges", Fail, err.Error(), ""})
	} else if !readAllStats {
		add(Result{"privileges", Fail, "role cannot see other roles' statements",
			"GRANT pg_monitor TO <this role>;"})
	} else {
		add(Result{"privileges", OK, "member of pg_read_all_stats", ""})
	}

	var super bool
	if err := q.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err == nil && super {
		add(Result{"least privilege", Warn, "connected as a superuser",
			"use a dedicated role with only pg_monitor (see deploy/target/setup.sql)"})
	}

	// A unix socket has no server address; loopback traffic never leaves the host.
	var ssl, local bool
	err := q.QueryRow(ctx, `
		SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false),
		       coalesce(inet_server_addr() <<= '127.0.0.0/8' OR inet_server_addr() = '::1', true)`).Scan(&ssl, &local)
	switch {
	case err != nil:
		add(Result{"encryption", Warn, err.Error(), ""})
	case ssl:
		add(Result{"encryption", OK, "TLS", ""})
	case local:
		add(Result{"encryption", OK, "local connection", ""})
	default:
		add(Result{"encryption", Warn, "connection is not encrypted; query texts and credentials cross the network in clear",
			"add sslmode=verify-full (with sslrootcert) to the DSN"})
	}

	var helper bool
	if err := q.QueryRow(ctx, `SELECT to_regprocedure('pgculprit.column_stats(name[],name[])') IS NOT NULL`).Scan(&helper); err != nil {
		add(Result{"column statistics helper", Fail, err.Error(), ""})
	} else if !helper {
		add(Result{"column statistics helper", Warn, "pgculprit.column_stats() not installed; stats limited to columns this role can SELECT",
			"run deploy/target/helper.sql as the table owner or an administrator"})
	} else {
		add(Result{"column statistics helper", OK, "installed", ""})
	}

	var queryID, trackPlanning string
	if err := q.QueryRow(ctx, `SELECT current_setting('compute_query_id'), current_setting('pg_stat_statements.track_planning', true)`).Scan(&queryID, &trackPlanning); err == nil {
		if queryID == "off" {
			add(Result{"compute_query_id", Warn, "off", "set compute_query_id = auto or on; plans from auto_explain are matched by query id"})
		} else {
			add(Result{"compute_query_id", OK, queryID, ""})
		}
		if trackPlanning != "on" {
			add(Result{"planning time", Warn, "pg_stat_statements.track_planning is off; planning time will read as zero",
				"optional: set pg_stat_statements.track_planning = on"})
		}
	}
	return out
}
