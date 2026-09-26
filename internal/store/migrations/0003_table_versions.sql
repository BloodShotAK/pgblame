-- Table sizes move constantly, so sampling them every cycle grew without
-- bound; keep a version only when the size shifts enough to matter.
DROP TABLE table_stat_samples;

CREATE TABLE table_stat_versions (
    id          bigserial PRIMARY KEY,
    target_id   bigint NOT NULL REFERENCES targets (id),
    dbname      text NOT NULL,
    schema_name text NOT NULL,
    table_name  text NOT NULL,
    reltuples   bigint NOT NULL,
    relpages    integer NOT NULL,
    valid_from  timestamptz NOT NULL,
    valid_to    timestamptz,
    end_reason  text CHECK (end_reason IN ('removed', 'modified'))
);
CREATE UNIQUE INDEX ON table_stat_versions (target_id, dbname, schema_name, table_name)
    WHERE valid_to IS NULL;
