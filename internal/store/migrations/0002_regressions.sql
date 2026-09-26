CREATE TABLE regressions (
    id                bigserial PRIMARY KEY,
    query_id          bigint NOT NULL REFERENCES queries (id),
    opened_at         timestamptz NOT NULL,
    last_seen_at      timestamptz NOT NULL,
    closed_at         timestamptz,
    kind              text NOT NULL,
    baseline_kind     text NOT NULL,
    baseline_ms       double precision NOT NULL,
    current_ms        double precision NOT NULL,
    peak_ms           double precision NOT NULL,
    extra_ms_per_hour double precision NOT NULL,
    calls             bigint NOT NULL,
    z                 double precision NOT NULL
);
CREATE UNIQUE INDEX ON regressions (query_id) WHERE closed_at IS NULL;
CREATE INDEX ON regressions (opened_at);
