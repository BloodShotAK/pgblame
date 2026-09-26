-- Run as a superuser or the provider's admin role. pg_stat_statements must be in shared_preload_libraries.

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- Local dev password only.
CREATE ROLE pgculprit LOGIN PASSWORD 'pgculprit';

GRANT pg_monitor TO pgculprit;
