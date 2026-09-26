-- SECURITY DEFINER wrapper so the pgculprit role can read planner stats without table access.
-- Leaves out histograms and most-common values, which contain real data. Install per database.

CREATE SCHEMA IF NOT EXISTS pgculprit;

DROP FUNCTION IF EXISTS pgculprit.column_stats();
DROP FUNCTION IF EXISTS pgculprit.column_stats(text[], text[]);

CREATE OR REPLACE FUNCTION pgculprit.column_stats(schemas name[], tables name[])
RETURNS TABLE (
    schemaname  name,
    tablename   name,
    attname     name,
    inherited   boolean,
    null_frac   double precision,
    n_distinct  double precision,
    correlation double precision
)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    -- One lookup per table: pg_stats is a security barrier view, and a filter
    -- joined against a list isn't pushed into it, so it would scan every column.
    FOR i IN 1 .. coalesce(array_length(tables, 1), 0) LOOP
        RETURN QUERY
            SELECT s.schemaname, s.tablename, s.attname, s.inherited,
                   s.null_frac::float8, s.n_distinct::float8, s.correlation::float8
            FROM pg_catalog.pg_stats s
            WHERE s.schemaname = schemas[i] AND s.tablename = tables[i];
    END LOOP;
END
$$;

REVOKE ALL ON FUNCTION pgculprit.column_stats(name[], name[]) FROM PUBLIC;
GRANT USAGE ON SCHEMA pgculprit TO pgculprit;
GRANT EXECUTE ON FUNCTION pgculprit.column_stats(name[], name[]) TO pgculprit;
