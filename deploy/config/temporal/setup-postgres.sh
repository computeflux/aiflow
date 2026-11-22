#!/bin/sh
set -eu

: "${POSTGRES_SEEDS:?required}"
: "${POSTGRES_USER:?required}"

P="${POSTGRES_PWD:-temporal}"
PLUGIN=postgres12
EP="${POSTGRES_SEEDS}"
PORT="${DB_PORT:-5432}"
SCHEMA_DIR=/etc/temporal/schema/postgresql/v12

echo 'Starting PostgreSQL schema setup...'
echo 'Waiting for PostgreSQL...'
nc -z -w 10 "$EP" "$PORT"
echo 'PostgreSQL is available'

T() { temporal-sql-tool --plugin "$PLUGIN" --ep "$EP" -u "$POSTGRES_USER" --pw "$P" -p "$PORT" "$@"; }

# temporal db
T --db temporal create 2>/dev/null || true
T --db temporal setup-schema -v 0.0 2>/dev/null || true
T --db temporal update-schema -d "$SCHEMA_DIR/temporal/versioned"

# visibility db  
T --db temporal_visibility create 2>/dev/null || true
T --db temporal_visibility setup-schema -v 0.0 2>/dev/null || true
T --db temporal_visibility update-schema -d "$SCHEMA_DIR/visibility/versioned"

echo 'PostgreSQL schema setup complete'
