#!/usr/bin/env bash
# One-off export of historical metadata tables to the trajectory bucket.
# Each table becomes backfill/<date>/<table>.jsonl.gz (one row_to_json per line).
#
# Needs: psql, aws CLI. Usage (see railway/README.md, "Backfill"):
#   DATABASE_URL=postgres://... BUCKET=... ENDPOINT=https://t3.storageapi.dev \
#   AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... ./railway/backfill.sh
set -euo pipefail

: "${DATABASE_URL:?}" "${BUCKET:?}" "${ENDPOINT:?}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-auto}"
prefix="${PREFIX:-trajectories/}backfill/$(date -u +%Y-%m-%d)"
tables="usage_logs ops_error_logs ops_retry_attempts ops_system_logs ops_system_metrics
ops_metrics_hourly ops_metrics_daily ops_alert_events ops_ingress_reject_aggregates"

for t in $tables; do
  if [ "$(psql "$DATABASE_URL" -Atc "select to_regclass('public.$t') is not null")" != t ]; then
    echo "skip $t (missing)"; continue
  fi
  echo "export $t"
  # -At + FETCH_COUNT streams rows unescaped (COPY text format would double backslashes).
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v FETCH_COUNT=10000 -Atc "select row_to_json(x) from $t x" \
    | gzip | aws s3 cp - "s3://$BUCKET/$prefix/$t.jsonl.gz" --endpoint-url "$ENDPOINT"
done
echo "done: s3://$BUCKET/$prefix/"
