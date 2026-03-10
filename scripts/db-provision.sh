#!/usr/bin/env bash
set -euo pipefail

# Usage: ./db-provision.sh <service-name> <postgres|redis> [db-name]
SVC_NAME="${1:?Usage: db-provision.sh <service-name> <postgres|redis> [db-name]}"
DB_TYPE="${2:?Usage: db-provision.sh <service-name> <postgres|redis> [db-name]}"
DB_NAME="${3:-${SVC_NAME}-db}"
URL="${AH_URL:?Set AH_URL}"
KEY="${AH_KEY:?Set AH_KEY}"

if [[ "$DB_TYPE" != "postgres" && "$DB_TYPE" != "redis" ]]; then
  echo "Error: type must be 'postgres' or 'redis'"
  exit 1
fi

# Find service
SVCS=$(curl -sf -H "Authorization: Bearer $KEY" "$URL/v1/services")
SERVICE_ID=$(echo "$SVCS" | python3 -c "
import json,sys
for s in json.load(sys.stdin):
    if s['name'] == '${SVC_NAME}':
        print(s['id'])
        break
" 2>/dev/null)

if [ -z "$SERVICE_ID" ]; then
  echo "Error: service '$SVC_NAME' not found"
  exit 1
fi

echo "Provisioning $DB_TYPE database '$DB_NAME'..."
IDEM_KEY=$(uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid)

DB=$(curl -sf -X POST "$URL/v1/databases" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d "{\"name\":\"$DB_NAME\",\"type\":\"$DB_TYPE\"}")

DB_ID=$(echo "$DB" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "  Database ID: $DB_ID"

# Poll for running
echo "  Waiting for database to start..."
for i in $(seq 1 12); do
  STATUS=$(curl -sf -H "Authorization: Bearer $KEY" "$URL/v1/databases/$DB_ID" \
    | grep -o '"status":"[^"]*"' | cut -d'"' -f4)
  [ "$STATUS" = "running" ] && break
  sleep 5
done

if [ "$STATUS" != "running" ]; then
  echo "Database failed to start"
  exit 1
fi

echo "  Status: running"

# Get connection string
CONN=$(curl -sf -H "Authorization: Bearer $KEY" \
  "$URL/v1/databases/$DB_ID/connection-string" \
  | grep -o '"connection_string":"[^"]*"' | cut -d'"' -f4)

# Set env var
if [ "$DB_TYPE" = "postgres" ]; then
  ENV_KEY="DATABASE_URL"
else
  ENV_KEY="REDIS_URL"
fi

echo "  Setting $ENV_KEY on service '$SVC_NAME'..."
curl -sf -X POST "$URL/v1/services/$SERVICE_ID/env" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "{\"$ENV_KEY\": \"$CONN\"}" > /dev/null

# Restart service
echo "  Restarting service..."
curl -sf -X POST "$URL/v1/services/$SERVICE_ID/restart" \
  -H "Authorization: Bearer $KEY" > /dev/null

echo "Done -- $DB_TYPE wired to '$SVC_NAME' as \$$ENV_KEY"
