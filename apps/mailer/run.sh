#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

export MAILER_PORT="${MAILER_PORT:-8003}"

echo "Building mailer service..."
go build -o bin/mailer .

echo "Starting mailer service on port $MAILER_PORT..."
exec ./bin/mailer
