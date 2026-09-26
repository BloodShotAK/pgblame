-- SECURITY DEFINER wrapper so the pgblame role can read planner stats without table access.
-- Leaves out histograms and most-common values, which contain real data. Install per database.

CREATE SCHEMA IF NOT EXISTS pgblame;

CREATE OR REPLACE FUNCTION pgblame.column_stats()
RETURNS TABLE (
    schemaname  name,
    tablename   name,
    attname     name,
    inherited   boolean,
    null_frac   double precision,
    n_distinct  double precision,
    correlation double precision
)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT schemaname, tablename, attname, inherited,
           null_frac::float8, n_distinct::float8, correlation::float8
    FROM pg_catalog.pg_stats
    WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
$$;

REVOKE ALL ON FUNCTION pgblame.column_stats() FROM PUBLIC;
GRANT USAGE ON SCHEMA pgblame TO pgblame;
GRANT EXECUTE ON FUNCTION pgblame.column_stats() TO pgblame;
