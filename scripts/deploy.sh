#!/usr/bin/env bash
set -euo pipefail

# Usage: ./deploy.sh <git-url-or-image> <service-name> [port]
SOURCE="${1:?Usage: deploy.sh <git-url-or-image> <service-name> [port]}"
NAME="${2:?Usage: deploy.sh <git-url-or-image> <service-name> [port]}"
PORT="${3:-3000}"
URL="${AH_URL:?Set AH_URL}"
KEY="${AH_KEY:?Set AH_KEY}"

is_git_url() { [[ "$1" == https://* ]]; }

echo "Deploying '$NAME'..."

# Create service
if is_git_url "$SOURCE"; then
  IMAGE="placeholder:latest"
else
  IMAGE="$SOURCE"
fi

IDEM_KEY=$(uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid)

SVC=$(curl -sf -X POST "$URL/v1/services" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d "{\"name\":\"$NAME\",\"image\":\"$IMAGE\",\"port\":$PORT,\"memory_mb\":512,\"cpu_count\":1}")

SERVICE_ID=$(echo "$SVC" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "  Service ID: $SERVICE_ID"

# If git URL, start build
if is_git_url "$SOURCE"; then
  echo "  Starting Nixpacks build from $SOURCE..."
  BUILD=$(curl -sf -X POST "$URL/v1/services/$SERVICE_ID/builds" \
    -H "Authorization: Bearer $KEY" \
    -H "Content-Type: application/json" \
    -d "{\"git_url\":\"$SOURCE\",\"branch\":\"main\"}")
  BUILD_ID=$(echo "$BUILD" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
  echo "  Build ID  : $BUILD_ID"
  echo "  Streaming build logs..."
  echo ""
  curl -sN -H "Authorization: Bearer $KEY" \
    "$URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
  echo ""
fi

# Poll for running
echo "  Waiting for service to start..."
for i in $(seq 1 120); do
  RESP=$(curl -sf -H "Authorization: Bearer $KEY" "$URL/v1/services/$SERVICE_ID")
  STATUS=$(echo "$RESP" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
  SVC_URL=$(echo "$RESP" | grep -o '"url":"[^"]*"' | head -1 | cut -d'"' -f4)
  if [ "$STATUS" = "running" ]; then
    echo "Running"
    echo "  URL: $SVC_URL"
    exit 0
  elif [ "$STATUS" = "failed" ]; then
    echo "Deploy failed"
    exit 1
  fi
  printf "  [%ds] %s\r" "$((i * 5))" "$STATUS"
  sleep 5
done

echo "Timed out waiting for service to start (10 min)"
exit 1
