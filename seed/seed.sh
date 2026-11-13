#!/bin/sh
# Applies the schema (idempotent) and loads the seed. Run via:
#   docker compose --profile seed run --rm seed
set -eu

: "${SEED_ROWS:=10000000}"
: "${SEED_USERS:=100000}"
: "${SEED_WHALES:=10}"

until pg_isready -q; do echo "waiting for database..."; sleep 1; done

start=$(date +%s)
psql -v ON_ERROR_STOP=1 -q -f /migrations/001_init.sql
psql -v ON_ERROR_STOP=1 \
     -v rows="$SEED_ROWS" -v users="$SEED_USERS" -v whales="$SEED_WHALES" \
     -f /seed/seed.sql
end=$(date +%s)
echo "Seed finished in $((end - start))s (rows=$SEED_ROWS users=$SEED_USERS whales=$SEED_WHALES)"
