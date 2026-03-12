---
name: agentic-hosting
description: Operate an agentic-hosting self-hosted PaaS server via REST API. Use when deploying apps, provisioning databases, managing services, or troubleshooting on an agentic-hosting instance.
---

## Quick Start

```bash
export AH_URL="https://<your-server>"   # or http://IP:8080
export AH_KEY="<keyid.secret>"          # from tenant registration
```

Then in Claude Code: `/status` · `/deploy <git-url> <name>` · `/db <name> postgres` · `/logs <name>`

---

# agentic-hosting Operator Skill

agentic-hosting is an agentic-first self-hosted PaaS. You operate it entirely via REST API — no web dashboard. This skill gives you everything needed to deploy apps, provision databases, and manage services.

## Server Setup

Use this section when bootstrapping `ah` on a fresh Linux server from scratch. Skip to **Setup (do this first)** if the server is already running.

### Prerequisites

- Ubuntu 20.04+ or Debian 11+
- Root or sudo access
- Ports 80, 443 open in firewall

### 1. Install Dependencies

```bash
# Docker
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker

# Go 1.22+
wget https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile.d/go.sh
source /etc/profile.d/go.sh

# gVisor (runsc)
curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" > /etc/apt/sources.list.d/gvisor.list
apt-get update && apt-get install -y runsc
runsc install

# Nixpacks
curl -sSL https://nixpacks.com/install.sh | bash
```

### 2. Clone and Build

```bash
cd /opt
git clone https://github.com/dennisonbertram/agentic-hosting
cd agentic-hosting
CGO_ENABLED=1 go build -o /usr/local/bin/ah ./cmd/ah/
```

### 3. Generate Secrets

```bash
# Master key (hex)
mkdir -p /var/lib/ah
openssl rand -hex 32 > /var/lib/ah/master.key

# Bootstrap token
BOOTSTRAP_TOKEN=$(openssl rand -hex 24)
echo "AH_BOOTSTRAP_TOKEN=$BOOTSTRAP_TOKEN" > /etc/default/ah
echo "Save this token: $BOOTSTRAP_TOKEN"
```

**Save the bootstrap token immediately — it cannot be recovered.**

### 4. Set Up Data Directory

```bash
mkdir -p /var/lib/ah
```

### 5. Create Systemd Service

Create `/etc/systemd/system/ah.service`:

```ini
[Unit]
Description=agentic-hosting daemon
After=docker.service
Requires=docker.service

[Service]
EnvironmentFile=/etc/default/ah
ExecStart=/usr/local/bin/ah --port 8080 --db-path /var/lib/ah/ah.db --master-key-path /var/lib/ah/master.key --dev
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now ah
```

### 6. Start Traefik and Local Registry

```bash
# Create traefik-public network
docker network create traefik-public

# Local registry
docker run -d --name paas-registry \
  --restart=unless-stopped \
  -p 127.0.0.1:5000:5000 \
  registry:2

# Traefik (create /etc/traefik/traefik.yml first — see docs)
mkdir -p /etc/traefik/dynamic /etc/traefik/certs
touch /etc/traefik/certs/acme.json && chmod 600 /etc/traefik/certs/acme.json

docker run -d --name paas-traefik \
  --restart=unless-stopped \
  --network traefik-public \
  -p 80:80 -p 443:443 -p 8090:8080 \
  -v /etc/traefik/traefik.yml:/etc/traefik/traefik.yml:ro \
  -v /etc/traefik/dynamic:/etc/traefik/dynamic:ro \
  -v /etc/traefik/certs:/etc/traefik/certs \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  traefik:latest
```

### 7. Verify

```bash
curl -s http://localhost:8080/v1/system/health
# → {"status":"ok"}
```

### 8. Register First Tenant and Get API Key

```bash
curl -X POST http://localhost:8080/v1/tenants/register \
  -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","email":"me@example.com"}'
# → {"tenant_id":"ten_...","api_key":"keyid.secret"}
```

### After Setup — Output to User

Once bootstrap is complete, output:

```
✓ agentic-hosting is running on <server-ip>

Add to your shell profile:
  export AH_URL=http://<server-ip>:8080
  export AH_KEY=<api-key>

Then run: /status
```

### Important Notes for Claude

- Always save the bootstrap token before proceeding — it cannot be recovered
- The master key at `/var/lib/ah/master.key` must be backed up — losing it means losing all encrypted DB credentials
- gVisor path is `/usr/bin/runsc` after `runsc install` (verify with `which runsc`)
- If Docker version is 29+, use `traefik:latest` not a pinned v3.x version
- Port 8080 is loopback-only by default — expose via Traefik for external access

---

## Setup (do this first)

Before using any ah commands, ensure you have:

```bash
export AH_URL="https://<your-domain>"   # or http://localhost:8080 in dev
export AH_KEY="<your-api-key>"          # format: keyid.secret
```

Verify connectivity:
```bash
curl -s $AH_URL/v1/system/health
# → {"status":"ok"}
```

## Authentication

All requests (except health) require:
```
Authorization: Bearer $AH_KEY
Content-Type: application/json
```

API keys are in format `keyid.secret`. If you don't have a key, see **Register a Tenant** below.

## Register a Tenant (one-time setup)

Requires the server bootstrap token (ask the server operator, or read from `/etc/default/ah` on the server):

```bash
curl -s -X POST $AH_URL/v1/tenants/register \
  -H "Content-Type: application/json" \
  -H "X-Bootstrap-Token: $AH_BOOTSTRAP_TOKEN" \
  -d '{"name": "my-tenant", "email": "me@example.com"}'
# → {"tenant_id": "...", "api_key": "keyid.secret"}
# SAVE the api_key — it won't be shown again
```

## Deploy a Service

### From a Docker image
```bash
curl -s -X POST $AH_URL/v1/services \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","image":"nginx:alpine","port":80,"memory_mb":256,"cpu_count":1}'
# Returns immediately with status "deploying"
```

### Poll until running (max 10 minutes)
```bash
SERVICE_ID="<id-from-above>"
for i in $(seq 1 120); do
  STATUS=$(curl -s -H "Authorization: Bearer $AH_KEY" \
    $AH_URL/v1/services/$SERVICE_ID | grep -o '"status":"[^"]*"' | cut -d'"' -f4)
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
SVC=$(curl -s -X POST $AH_URL/v1/services \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","image":"placeholder:latest","port":3000}')
SERVICE_ID=$(echo $SVC | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Start build
BUILD=$(curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/builds \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"git_url":"https://github.com/org/repo","branch":"main"}')
BUILD_ID=$(echo $BUILD | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Stream build logs
curl -s -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
```

## Provision a Database

```bash
# Create (takes up to 30s — use idempotency key to safely retry)
DB=$(curl -s -X POST $AH_URL/v1/databases \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"mydb","type":"postgres"}')  # or "redis"
DB_ID=$(echo $DB | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Get connection string
CONN=$(curl -s -H "Authorization: Bearer $AH_KEY" \
  $AH_URL/v1/databases/$DB_ID/connection-string \
  | grep -o '"connection_string":"[^"]*"' | cut -d'"' -f4)

# Wire to service as env var
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/env \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"DATABASE_URL\": \"$CONN\"}"

# Restart service to pick up new env
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/restart \
  -H "Authorization: Bearer $AH_KEY"
```

## Common Operations

```bash
# List all services
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/services | python3 -m json.tool

# List all databases
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/databases | python3 -m json.tool

# Stop / start / restart a service
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/stop -H "Authorization: Bearer $AH_KEY"
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/start -H "Authorization: Bearer $AH_KEY"
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/restart -H "Authorization: Bearer $AH_KEY"

# Reset circuit breaker (after 5 crashes in 10 minutes)
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/reset -H "Authorization: Bearer $AH_KEY"

# View / set / delete env vars
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/services/$SERVICE_ID/env?reveal=true"
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/env \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"KEY": "value", "OTHER": "value2"}'
curl -s -X DELETE $AH_URL/v1/services/$SERVICE_ID/env/KEY \
  -H "Authorization: Bearer $AH_KEY"

# Delete service or database
curl -s -X DELETE $AH_URL/v1/services/$SERVICE_ID -H "Authorization: Bearer $AH_KEY"
curl -s -X DELETE $AH_URL/v1/databases/$DB_ID -H "Authorization: Bearer $AH_KEY"

# Detailed system health (disk, docker, gVisor)
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/system/health/detailed | python3 -m json.tool

# Create a named API key (e.g. for CI)
curl -s -X POST $AH_URL/v1/auth/keys \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"name":"ci-key","expires_in":2592000}'

# Revoke a key
curl -s -X DELETE $AH_URL/v1/auth/keys/$KEY_ID -H "Authorization: Bearer $AH_KEY"
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
