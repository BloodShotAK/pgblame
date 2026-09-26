-- Raw samples go into daily partitions so retention is a DROP rather than a
-- DELETE that bloats the table; long-term baselines use hourly rollups.
ALTER TABLE query_samples RENAME TO query_samples_unpartitioned;
ALTER INDEX query_samples_pkey RENAME TO query_samples_unpartitioned_pkey;
ALTER INDEX query_samples_interval_end_idx RENAME TO query_samples_unpartitioned_interval_end_idx;

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
) PARTITION BY RANGE (interval_end);
CREATE INDEX ON query_samples (interval_end);

-- Partition days are UTC, matching Store.Maintain.
DO $$
DECLARE
    d date;
BEGIN
    FOR d IN
        SELECT generate_series(
            least((SELECT (min(interval_end) AT TIME ZONE 'UTC')::date FROM query_samples_unpartitioned),
                  (now() AT TIME ZONE 'UTC')::date - 1),
            (now() AT TIME ZONE 'UTC')::date + 2,
            interval '1 day')::date
    LOOP
        EXECUTE format('CREATE TABLE %I PARTITION OF query_samples FOR VALUES FROM (%L) TO (%L)',
                       'query_samples_' || to_char(d, 'YYYYMMDD'),
                       d::timestamp AT TIME ZONE 'UTC', (d + 1)::timestamp AT TIME ZONE 'UTC');
    END LOOP;
END $$;

INSERT INTO query_samples SELECT * FROM query_samples_unpartitioned;
DROP TABLE query_samples_unpartitioned;

CREATE TABLE query_samples_hourly (
    query_id         bigint NOT NULL REFERENCES queries (id),
    hour_start       timestamptz NOT NULL,
    calls            bigint NOT NULL,
    exec_ms          double precision NOT NULL,
    rows             bigint NOT NULL,
    shared_blks_hit  bigint NOT NULL,
    shared_blks_read bigint NOT NULL,
    PRIMARY KEY (query_id, hour_start)
);
CREATE INDEX ON query_samples_hourly (hour_start);
