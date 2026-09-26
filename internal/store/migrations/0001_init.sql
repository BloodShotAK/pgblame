CREATE TABLE targets (
    id         bigserial PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- OIDs are unsigned 32-bit, hence bigint.
CREATE TABLE queries (
    id         bigserial PRIMARY KEY,
    target_id  bigint NOT NULL REFERENCES targets (id),
    dbid       bigint NOT NULL,
    userid     bigint NOT NULL,
    queryid    bigint NOT NULL,
    toplevel   boolean NOT NULL,
    dbname     text NOT NULL,
    username   text NOT NULL,
    -- NULL if the extension lost the text
    query_text text,
    first_seen timestamptz NOT NULL,
    last_seen  timestamptz NOT NULL,
    UNIQUE (target_id, dbid, userid, queryid, toplevel)
);

CREATE TABLE pgss_snapshots (
    id                 bigserial PRIMARY KEY,
    target_id          bigint NOT NULL REFERENCES targets (id),
    taken_at           timestamptz NOT NULL,
    server_version_num integer NOT NULL,
    stats_reset        timestamptz,
    dealloc            bigint NOT NULL,
    entries            integer NOT NULL,
    interval_start     timestamptz,
    samples            integer,
    global_reset       boolean,
    eviction_suspect   boolean,
    dealloc_delta      bigint,
    restarted          integer,
    evicted            integer
);
CREATE INDEX ON pgss_snapshots (target_id, taken_at);

-- Raw deltas rather than means, so rollups can sum them.
CREATE TABLE query_samples (
    query_id            bigint NOT NULL REFERENCES queries (id),
    interval_start      timestamptz NOT NULL,
    interval_end        timestamptz NOT NULL,
    partial             boolean NOT NULL,
    calls               bigint NOT NULL,
    plans               bigint NOT NULL,
    exec_ms             double precision NOT NULL,
    plan_ms             double precision NOT NULL,
    rows                bigint NOT NULL,
    shared_blks_hit     bigint NOT NULL,
    shared_blks_read    bigint NOT NULL,
    shared_blks_dirtied bigint NOT NULL,
    shared_blks_written bigint NOT NULL,
    temp_blks_read      bigint NOT NULL,
    temp_blks_written   bigint NOT NULL,
    wal_bytes           bigint NOT NULL,
    PRIMARY KEY (query_id, interval_end)
);
CREATE INDEX ON query_samples (interval_end);

-- Catalog history is stored as validity ranges: a new row only when something changes.

CREATE TABLE index_versions (
    id          bigserial PRIMARY KEY,
    target_id   bigint NOT NULL REFERENCES targets (id),
    dbname      text NOT NULL,
    schema_name text NOT NULL,
    table_name  text NOT NULL,
    index_name  text NOT NULL,
    definition  text NOT NULL,
    is_valid    boolean NOT NULL,
    is_ready    boolean NOT NULL,
    is_unique   boolean NOT NULL,
    is_primary  boolean NOT NULL,
    valid_from  timestamptz NOT NULL,
    valid_to    timestamptz,
    end_reason  text CHECK (end_reason IN ('removed', 'modified'))
);
CREATE UNIQUE INDEX ON index_versions (target_id, dbname, schema_name, table_name, index_name)
    WHERE valid_to IS NULL;

CREATE TABLE setting_versions (
    id         bigserial PRIMARY KEY,
    target_id  bigint NOT NULL REFERENCES targets (id),
    dbname     text NOT NULL,
    name       text NOT NULL,
    value      text NOT NULL,
    unit       text NOT NULL,
    source     text NOT NULL,
    valid_from timestamptz NOT NULL,
    valid_to   timestamptz,
    end_reason text CHECK (end_reason IN ('removed', 'modified'))
);
CREATE UNIQUE INDEX ON setting_versions (target_id, dbname, name)
    WHERE valid_to IS NULL;

CREATE TABLE column_stat_versions (
    id          bigserial PRIMARY KEY,
    target_id   bigint NOT NULL REFERENCES targets (id),
    dbname      text NOT NULL,
    schema_name text NOT NULL,
    table_name  text NOT NULL,
    column_name text NOT NULL,
    inherited   boolean NOT NULL,
    null_frac   double precision NOT NULL,
    n_distinct  double precision NOT NULL,
    correlation double precision,
    valid_from  timestamptz NOT NULL,
    valid_to    timestamptz,
    end_reason  text CHECK (end_reason IN ('removed', 'modified'))
);
CREATE UNIQUE INDEX ON column_stat_versions (target_id, dbname, schema_name, table_name, column_name, inherited)
    WHERE valid_to IS NULL;

CREATE TABLE table_stat_samples (
    target_id        bigint NOT NULL REFERENCES targets (id),
    dbname           text NOT NULL,
    taken_at         timestamptz NOT NULL,
    schema_name      text NOT NULL,
    table_name       text NOT NULL,
    reltuples        bigint NOT NULL,
    relpages         integer NOT NULL,
    n_live_tup       bigint NOT NULL,
    n_dead_tup       bigint NOT NULL,
    last_analyze     timestamptz,
    last_autoanalyze timestamptz,
    PRIMARY KEY (target_id, dbname, schema_name, table_name, taken_at)
);
