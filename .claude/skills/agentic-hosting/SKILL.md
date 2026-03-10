---
name: agentic-hosting
description: Operate an agentic-hosting self-hosted PaaS server via REST API. Use when deploying apps, provisioning databases, managing services, or troubleshooting on an agentic-hosting instance.
---

# agentic-hosting Operator Skill

agentic-hosting is an agentic-first self-hosted PaaS. You operate it entirely via REST API — no web dashboard. This skill gives you everything needed to deploy apps, provision databases, and manage services.

## Setup (do this first)

Before using any paasd commands, ensure you have:

```bash
export PAASD_URL="https://<your-domain>"   # or http://localhost:8080 in dev
export PAASD_KEY="<your-api-key>"          # format: keyid.secret
```

Verify connectivity:
```bash
curl -s $PAASD_URL/v1/system/health
# → {"status":"ok"}
```

## Authentication

All requests (except health) require:
```
Authorization: Bearer $PAASD_KEY
Content-Type: application/json
```

API keys are in format `keyid.secret`. If you don't have a key, see **Register a Tenant** below.

## Register a Tenant (one-time setup)

Requires the server bootstrap token (ask the server operator, or read from `/etc/default/paasd` on the server):

```bash
curl -s -X POST $PAASD_URL/v1/tenants/register \
  -H "Content-Type: application/json" \
  -H "X-Bootstrap-Token: $PAASD_BOOTSTRAP_TOKEN" \
  -d '{"name": "my-tenant", "email": "me@example.com"}'
# → {"tenant_id": "...", "api_key": "keyid.secret"}
# SAVE the api_key — it won't be shown again
```

## Deploy a Service

### From a Docker image
```bash
curl -s -X POST $PAASD_URL/v1/services \
  -H "Authorization: Bearer $PAASD_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","image":"nginx:alpine","port":80,"memory_mb":256,"cpu_count":1}'
# Returns immediately with status "deploying"
```

### Poll until running (max 10 minutes)
```bash
SERVICE_ID="<id-from-above>"
for i in $(seq 1 120); do
  STATUS=$(curl -s -H "Authorization: Bearer $PAASD_KEY" \
    $PAASD_URL/v1/services/$SERVICE_ID | grep -o '"status":"[^"]*"' | cut -d'"' -f4)
  echo "[$i] $STATUS"
  [ "$STATUS" = "running" ] && break
  [ "$STATUS" = "failed" ] && { echo "Deploy failed"; break; }
  sleep 5
done
```

### Build from git (Nixpacks — zero config)
Supported hosts: GitHub, GitLab, Bitbucket, sr.ht, Codeberg (HTTPS only)

```bash
# First create the service
SVC=$(curl -s -X POST $PAASD_URL/v1/services \
  -H "Authorization: Bearer $PAASD_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","image":"placeholder:latest","port":3000}')
SERVICE_ID=$(echo $SVC | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Start build
BUILD=$(curl -s -X POST $PAASD_URL/v1/services/$SERVICE_ID/builds \
  -H "Authorization: Bearer $PAASD_KEY" \
  -H "Content-Type: application/json" \
  -d '{"git_url":"https://github.com/org/repo","branch":"main"}')
BUILD_ID=$(echo $BUILD | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Stream build logs
curl -s -H "Authorization: Bearer $PAASD_KEY" \
  "$PAASD_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
```

## Provision a Database

```bash
# Create (takes up to 30s — use idempotency key to safely retry)
DB=$(curl -s -X POST $PAASD_URL/v1/databases \
  -H "Authorization: Bearer $PAASD_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"mydb","type":"postgres"}')  # or "redis"
DB_ID=$(echo $DB | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Get connection string
CONN=$(curl -s -H "Authorization: Bearer $PAASD_KEY" \
  $PAASD_URL/v1/databases/$DB_ID/connection-string \
  | grep -o '"connection_string":"[^"]*"' | cut -d'"' -f4)

# Wire to service as env var
curl -s -X POST $PAASD_URL/v1/services/$SERVICE_ID/env \
  -H "Authorization: Bearer $PAASD_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"DATABASE_URL\": \"$CONN\"}"

# Restart service to pick up new env
curl -s -X POST $PAASD_URL/v1/services/$SERVICE_ID/restart \
  -H "Authorization: Bearer $PAASD_KEY"
```

## Common Operations

```bash
# List all services
curl -s -H "Authorization: Bearer $PAASD_KEY" $PAASD_URL/v1/services | python3 -m json.tool

# List all databases
curl -s -H "Authorization: Bearer $PAASD_KEY" $PAASD_URL/v1/databases | python3 -m json.tool

# Stop / start / restart a service
curl -s -X POST $PAASD_URL/v1/services/$SERVICE_ID/stop -H "Authorization: Bearer $PAASD_KEY"
curl -s -X POST $PAASD_URL/v1/services/$SERVICE_ID/start -H "Authorization: Bearer $PAASD_KEY"
curl -s -X POST $PAASD_URL/v1/services/$SERVICE_ID/restart -H "Authorization: Bearer $PAASD_KEY"

# Reset circuit breaker (after 5 crashes in 10 minutes)
curl -s -X POST $PAASD_URL/v1/services/$SERVICE_ID/reset -H "Authorization: Bearer $PAASD_KEY"

# View / set / delete env vars
curl -s -H "Authorization: Bearer $PAASD_KEY" "$PAASD_URL/v1/services/$SERVICE_ID/env?reveal=true"
curl -s -X POST $PAASD_URL/v1/services/$SERVICE_ID/env \
  -H "Authorization: Bearer $PAASD_KEY" -H "Content-Type: application/json" \
  -d '{"KEY": "value", "OTHER": "value2"}'
curl -s -X DELETE $PAASD_URL/v1/services/$SERVICE_ID/env/KEY \
  -H "Authorization: Bearer $PAASD_KEY"

# Delete service or database
curl -s -X DELETE $PAASD_URL/v1/services/$SERVICE_ID -H "Authorization: Bearer $PAASD_KEY"
curl -s -X DELETE $PAASD_URL/v1/databases/$DB_ID -H "Authorization: Bearer $PAASD_KEY"

# Detailed system health (disk, docker, gVisor)
curl -s -H "Authorization: Bearer $PAASD_KEY" $PAASD_URL/v1/system/health/detailed | python3 -m json.tool

# Create a named API key (e.g. for CI)
curl -s -X POST $PAASD_URL/v1/auth/keys \
  -H "Authorization: Bearer $PAASD_KEY" -H "Content-Type: application/json" \
  -d '{"name":"ci-key","expires_in":2592000}'

# Revoke a key
curl -s -X DELETE $PAASD_URL/v1/auth/keys/$KEY_ID -H "Authorization: Bearer $PAASD_KEY"
```

## Error Handling

| Error | Meaning | Fix |
|-------|---------|-----|
| `401 Unauthorized` | Bad or revoked API key | Check key format: `keyid.secret` |
| `422 Unprocessable Entity` | Validation failed or duplicate | Check body; name/email may already exist |
| `429 Too Many Requests` | Rate limited | Back off: per-tenant 100/s, global 500/s |
| `503 Service Unavailable` | Disk >90% or Docker down | Check `GET /v1/system/health/detailed` |
| Service stuck in `deploying` | Deploy timeout or Docker issue | Check detailed health; delete and retry |
| `circuit_open: true` | 5 crashes in 10 minutes | Fix the app, then `POST .../reset` |
| Build `failed` | Nixpacks error | Check build logs with `?follow=true` |

## Important Limits

| Resource | Limit |
|----------|-------|
| Databases per tenant | 3 |
| API keys per tenant | 20 |
| Env vars per service | 100 |
| Request body | 1 MB |
| Rate limit (per tenant) | 100 req/s, burst 200 |
| Rate limit (global) | 500 req/s, burst 1000 |
| Disk warn threshold | 80% |
| Disk block threshold | 90% |
| Deploy timeout | 10 minutes |
| Build log max size | 5 MB |

## Idempotency

Add `Idempotency-Key: <uuid>` to any POST/PUT/DELETE to make it safe to retry:
```bash
-H "Idempotency-Key: $(uuidgen)"
```
Same key + same tenant + same endpoint = same result, no duplicate resource created.

## GitHub Issues (known gaps)

- No runtime log streaming yet (#11) — build logs work, container stdout/stderr doesn't
- No API key recovery if all keys lost (#12) — requires server-side recovery
- Custom domain routing not yet supported (#14) — services get UUID-based hostnames
