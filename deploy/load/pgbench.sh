#!/bin/sh
set -eu

if ! psql -tAc "SELECT 1 FROM pg_tables WHERE tablename = 'pgbench_accounts'" | grep -q 1; then
  pgbench --initialize --scale="${SCALE:-10}" --foreign-keys
fi

exec pgbench --client="${CLIENTS:-4}" --jobs=2 --rate="${RATE:-100}" \
  --time="${DURATION:-86400}" --progress=60
