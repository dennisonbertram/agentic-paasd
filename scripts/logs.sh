#!/usr/bin/env bash
set -euo pipefail

# Usage: ./logs.sh <service-name> [build-id]
NAME="${1:?Usage: logs.sh <service-name> [build-id]}"
BUILD_ID="${2:-}"
URL="${AH_URL:?Set AH_URL}"
KEY="${AH_KEY:?Set AH_KEY}"

# Find service by name
SVCS=$(curl -sf -H "Authorization: Bearer $KEY" "$URL/v1/services")
SERVICE_ID=$(echo "$SVCS" | python3 -c "
import json,sys
svcs = json.load(sys.stdin)
for s in svcs:
    if s['name'] == '${NAME}':
        print(s['id'])
        break
" 2>/dev/null)

if [ -z "$SERVICE_ID" ]; then
  echo "Error: service '$NAME' not found"
  exit 1
fi

# Get build ID if not provided
if [ -z "$BUILD_ID" ]; then
  BUILDS=$(curl -sf -H "Authorization: Bearer $KEY" "$URL/v1/services/$SERVICE_ID/builds")
  BUILD_ID=$(echo "$BUILDS" | python3 -c "
import json,sys
builds = json.load(sys.stdin)
if builds:
    print(builds[0]['id'])
" 2>/dev/null)
fi

if [ -z "$BUILD_ID" ]; then
  echo "No builds found for service '$NAME'"
  exit 1
fi

echo "Streaming build logs for '$NAME' (build: $BUILD_ID)..."
echo ""
curl -sN -H "Authorization: Bearer $KEY" \
  "$URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
