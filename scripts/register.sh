#!/usr/bin/env bash
set -euo pipefail

# Usage: AH_BOOTSTRAP_TOKEN=<token> ./register.sh <name> <email>
# Registers a new tenant and saves credentials to ~/.ah

NAME="${1:?Usage: register.sh <name> <email>}"
EMAIL="${2:?Usage: register.sh <name> <email>}"
URL="${AH_URL:?Set AH_URL}"
TOKEN="${AH_BOOTSTRAP_TOKEN:?Set AH_BOOTSTRAP_TOKEN}"

echo "Registering tenant '$NAME'..."

RESP=$(curl -sf -X POST "$URL/v1/tenants/register" \
  -H "Content-Type: application/json" \
  -H "X-Bootstrap-Token: $TOKEN" \
  -d "{\"name\": \"$NAME\", \"email\": \"$EMAIL\"}")

API_KEY=$(echo "$RESP" | grep -o '"api_key":"[^"]*"' | cut -d'"' -f4)
TENANT_ID=$(echo "$RESP" | grep -o '"tenant_id":"[^"]*"' | cut -d'"' -f4)

if [ -z "$API_KEY" ]; then
  echo "Error: registration failed"
  echo "$RESP"
  exit 1
fi

echo ""
echo "Registered successfully"
echo "  Tenant ID : $TENANT_ID"
echo "  API Key   : $API_KEY"
echo ""
echo "Add to your shell profile:"
echo "  export AH_URL=\"$URL\""
echo "  export AH_KEY=\"$API_KEY\""
