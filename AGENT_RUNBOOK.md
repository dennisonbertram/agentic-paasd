# ah Agent Runbook

## Overview

agentic-hosting (`ah`) is a self-hosted Platform-as-a-Service running on a single Linux server. It builds and runs containerized applications using Nixpacks (from Git sources) or pre-built Docker images. It also provisions PostgreSQL databases and manages environment variables per service.

You are an AI agent operating ah via its REST API. There is no dashboard — you work entirely through HTTP calls. Every action you take is a curl command or equivalent HTTP request. This runbook tells you exactly what to call, what to expect back, and what to do when things go wrong.

---

## Prerequisites

Before you start, you need:

1. **API base URL**: `https://<your-domain>` (production, via Traefik) or `http://localhost:8080` (local dev with `--dev` flag -- no HTTPS enforcement)
2. **API key**: A bearer token for your tenant. Set it as `API_KEY` in your shell.
3. **Bootstrap token** (only needed when registering a new tenant): Read it from `/etc/default/ah` on the server as `AH_BOOTSTRAP_TOKEN`.

Set these in your shell before running any commands:

```bash
BASE_URL="https://<your-domain>"
API_KEY="your-api-key-here"
```

All authenticated requests require:
```
Authorization: Bearer $API_KEY
Content-Type: application/json
```

---

## Quick Reference — Common curl Commands

These are ready-to-run commands. Set `$BASE_URL`, `$API_KEY`, `$SERVICE_ID`, `$DB_ID` before using.

```bash
# Health check (no auth required)
curl -s "$BASE_URL/v1/system/health" | jq .

# Detailed health — disk, docker, gVisor status (auth required)
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/system/health/detailed" | jq .

# Register a new tenant (requires bootstrap token)
curl -s -X POST "$BASE_URL/v1/tenants/register" \
  -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-tenant"}' | jq .

# Create an API key
curl -s -X POST "$BASE_URL/v1/auth/keys" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-key"}' | jq .

# List API keys
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/auth/keys" | jq .

# Deploy a service from a Docker image
curl -s -X POST "$BASE_URL/v1/services" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-app", "image": "nginx:latest", "port": 80}' | jq .

# List all services
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/services" | jq .

# Get a specific service
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/services/$SERVICE_ID" | jq .

# Start a build from Git
curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/builds" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"git_url": "https://github.com/owner/repo", "branch": "main"}' | jq .

# List builds for a service
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/services/$SERVICE_ID/builds" | jq .

# Get build status
curl -s -H "Authorization: Bearer $API_KEY" \
  "$BASE_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID" | jq .

# Stream build logs
curl -s -H "Authorization: Bearer $API_KEY" \
  "$BASE_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs"

# Create a Postgres database
curl -s -X POST "$BASE_URL/v1/databases" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-db", "type": "postgres"}' | jq .

# List databases
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/databases" | jq .

# Get database connection string (decrypted)
curl -s -H "Authorization: Bearer $API_KEY" \
  "$BASE_URL/v1/databases/$DB_ID/connection-string" | jq .

# Set environment variables on a service
curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/env" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"DATABASE_URL": "postgres://...", "NODE_ENV": "production"}' | jq .

# List env vars (masked)
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/services/$SERVICE_ID/env" | jq .

# List env vars (revealed)
curl -s -H "Authorization: Bearer $API_KEY" \
  "$BASE_URL/v1/services/$SERVICE_ID/env?reveal=true" | jq .

# Restart a service
curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/restart" \
  -H "Authorization: Bearer $API_KEY" | jq .

# Reset circuit breaker
curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/reset" \
  -H "Authorization: Bearer $API_KEY" | jq .
```

---

## Task: Register a New Tenant

You only do this once per tenant. If you already have an API key, skip to the next task.

### Step 1 — Get the Bootstrap Token

If you have SSH access to the server, read the token:

```bash
ssh root@<your-server-ip> "grep AH_BOOTSTRAP_TOKEN /etc/default/ah | cut -d= -f2"
```

Store it:
```bash
BOOTSTRAP_TOKEN="the-token-value-here"
```

### Step 2 — Register the Tenant

Send the registration request with the bootstrap token in the `X-Bootstrap-Token` header:

```bash
curl -s -X POST "$BASE_URL/v1/tenants/register" \
  -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-tenant"}' | jq .
```

A successful response looks like:

```json
{
  "id": "ten_abc123",
  "name": "my-tenant",
  "api_key": "pk_live_xxxxxxxxxxxxxxxxxxxxxxxx",
  "created_at": "2026-03-10T12:00:00Z"
}
```

### Step 3 — Save Your API Key

You will NOT be able to retrieve the `api_key` again. Save it immediately:

```bash
API_KEY="pk_live_xxxxxxxxxxxxxxxxxxxxxxxx"
```

Store it wherever your agent persists credentials (environment variable, secrets store, etc.).

### What Can Go Wrong

- **Rate limit hit**: You will get `429 Too Many Requests` if more than 5 registrations come from your IP in one hour, or more than 20 globally in one hour. Wait and retry.
- **401 on registration**: The bootstrap token is wrong. Re-read it from `/etc/default/ah`.

---

## Task: Deploy an Application from a Docker Image

Use this when you have a pre-built Docker image (from Docker Hub, ghcr.io, etc.) and want to run it immediately without a build step.

### Step 1 — Create the Service

```bash
SERVICE_ID=$(curl -s -X POST "$BASE_URL/v1/services" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-app",
    "image": "nginx:latest",
    "port": 80
  }' | jq -r '.id')

echo "Service ID: $SERVICE_ID"
```

The response will include a `url` field — that is the public URL for your service.

### Step 2 — Poll Until Running

The service starts deploying asynchronously. Poll its status until it reaches `running`:

```bash
while true; do
  STATUS=$(curl -s -H "Authorization: Bearer $API_KEY" \
    "$BASE_URL/v1/services/$SERVICE_ID" | jq -r '.status')
  echo "Status: $STATUS"
  case "$STATUS" in
    running)
      echo "Service is up."
      break
      ;;
    failed|circuit_open)
      echo "Service failed. Check logs."
      break
      ;;
    deploying|starting)
      sleep 5
      ;;
    *)
      echo "Unexpected status: $STATUS"
      sleep 5
      ;;
  esac
done
```

Typical status flow: `created` → `deploying` → `running`

### Step 3 — Verify the URL

```bash
SERVICE_URL=$(curl -s -H "Authorization: Bearer $API_KEY" \
  "$BASE_URL/v1/services/$SERVICE_ID" | jq -r '.url')
echo "Service URL: $SERVICE_URL"
curl -s -o /dev/null -w "%{http_code}" "$SERVICE_URL"
```

A 200 response confirms the service is reachable.

### Setting Environment Variables Before Deploy

If your application needs environment variables set before it starts, set them right after creating the service and before it reaches `running`. Use `POST /v1/services/{id}/env` with a JSON object:

```bash
curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/env" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "PORT": "8080",
    "NODE_ENV": "production",
    "SECRET_KEY": "my-secret"
  }' | jq .
```

Then restart the service so it picks up the new values:

```bash
curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/restart" \
  -H "Authorization: Bearer $API_KEY" | jq .
```

---

## Task: Build and Deploy from Git

Use this when you have application source code in a Git repository and want ah to build it using Nixpacks.

Supported Git hosts: GitHub, GitLab, Bitbucket, sr.ht, Codeberg. URLs must be HTTPS, not SSH.

### Step 1 — Create the Service (Without an Image)

Create the service first. You can omit the `image` field or pass an empty string — you'll supply the built image after the build completes.

```bash
SERVICE_ID=$(curl -s -X POST "$BASE_URL/v1/services" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-git-app",
    "port": 3000
  }' | jq -r '.id')

echo "Service ID: $SERVICE_ID"
```

### Step 2 — Start a Build

Trigger a Nixpacks build from your Git repository:

```bash
BUILD_ID=$(curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/builds" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "git_url": "https://github.com/owner/repo",
    "branch": "main"
  }' | jq -r '.id')

echo "Build ID: $BUILD_ID"
```

### Step 3 — Poll Build Status

Builds run asynchronously. Poll until `succeeded` or `failed`:

```bash
while true; do
  BUILD=$(curl -s -H "Authorization: Bearer $API_KEY" \
    "$BASE_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID")
  STATUS=$(echo "$BUILD" | jq -r '.status')
  echo "Build status: $STATUS"
  case "$STATUS" in
    succeeded)
      IMAGE=$(echo "$BUILD" | jq -r '.image')
      echo "Built image: $IMAGE"
      break
      ;;
    failed|cancelled)
      echo "Build failed."
      break
      ;;
    building|pending)
      sleep 10
      ;;
    *)
      echo "Unexpected build status: $STATUS"
      sleep 10
      ;;
  esac
done
```

### Step 4 — Stream Build Logs (if Build Failed)

If the build failed, get the logs to understand why:

```bash
curl -s -H "Authorization: Bearer $API_KEY" \
  "$BASE_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs"
```

Logs stream as newline-delimited text. Common failure causes:
- Bad Git URL or private repository without credentials
- Missing `package.json`, `requirements.txt`, or other build manifest
- Nixpacks cannot detect the language/framework
- Build exceeded the timeout

### Step 5 — Deploy the Built Image

Once the build succeeds, update the service to use the built image and restart it:

```bash
curl -s -X PATCH "$BASE_URL/v1/services/$SERVICE_ID" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"image\": \"$IMAGE\"}" | jq .

curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/restart" \
  -H "Authorization: Bearer $API_KEY" | jq .
```

Then poll for `running` status as in the Docker image task above.

### Using an Idempotency Key for Builds

If you are unsure whether a build request was received (network timeout, etc.), retry with the same idempotency key to avoid starting duplicate builds:

```bash
IDEMPOTENCY_KEY=$(uuidgen)  # or any stable UUID for this operation

curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/builds" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $IDEMPOTENCY_KEY" \
  -d '{"git_url": "https://github.com/owner/repo", "branch": "main"}' | jq .
```

Sending the same idempotency key again returns the original response without starting a new build.

---

## Task: Provision a Database and Connect It to a Service

Use this to create a PostgreSQL database and wire it to a running service via an environment variable.

### Step 1 — Create the Database

Database creation can take up to 30 seconds. Do not use a short HTTP timeout. Use an idempotency key in case you need to retry:

```bash
IDEMPOTENCY_KEY=$(uuidgen)

DB_ID=$(curl -s -X POST "$BASE_URL/v1/databases" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $IDEMPOTENCY_KEY" \
  -d '{
    "name": "my-app-db",
    "type": "postgres"
  }' | jq -r '.id')

echo "Database ID: $DB_ID"
```

If the request times out, wait 15 seconds and retry with the same `$IDEMPOTENCY_KEY`. The server deduplicates the request and returns the existing database rather than creating a second one.

### Step 2 — Get the Connection String

```bash
CONNECTION_STRING=$(curl -s -H "Authorization: Bearer $API_KEY" \
  "$BASE_URL/v1/databases/$DB_ID/connection-string" | jq -r '.connection_string')

echo "Connection string: $CONNECTION_STRING"
```

The connection string is returned decrypted. It will be in standard PostgreSQL URI format:
```
postgres://username:password@hostname:5432/dbname
```

### Step 3 — Set the Connection String as an Environment Variable

Set it on your service. Most frameworks expect `DATABASE_URL`:

```bash
curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/env" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"DATABASE_URL\": \"$CONNECTION_STRING\"}" | jq .
```

### Step 4 — Restart the Service

The service must restart to pick up the new environment variable:

```bash
curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/restart" \
  -H "Authorization: Bearer $API_KEY" | jq .
```

Wait for `running` status before considering the task complete.

### Tenant Limit

Each tenant can have at most 3 databases. If you need more, you will receive a `409 Conflict` or similar error. Check your existing databases first:

```bash
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/databases" | jq '. | length'
```

---

## Task: Rotate API Keys

Do this when you suspect a key is compromised, or on a scheduled rotation policy.

### Step 1 — Create a New Key

```bash
NEW_KEY_RESPONSE=$(curl -s -X POST "$BASE_URL/v1/auth/keys" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "rotated-key-2026-03"}')

NEW_KEY=$(echo "$NEW_KEY_RESPONSE" | jq -r '.key')
NEW_KEY_ID=$(echo "$NEW_KEY_RESPONSE" | jq -r '.id')

echo "New key: $NEW_KEY"
echo "New key ID: $NEW_KEY_ID"
```

The `key` value will NOT be shown again after this response. Save it immediately.

### Step 2 — Verify the New Key Works

Test the new key before revoking the old one:

```bash
curl -s -H "Authorization: Bearer $NEW_KEY" "$BASE_URL/v1/tenant" | jq .
```

You should see your tenant info. If you get `401`, something went wrong — do not proceed.

### Step 3 — Note the Old Key ID

List your keys and find the ID of the key you want to revoke:

```bash
curl -s -H "Authorization: Bearer $NEW_KEY" "$BASE_URL/v1/auth/keys" | jq .
```

The response lists all keys with their IDs and names. Find the old key by name or creation date.

### Step 4 — Revoke the Old Key

```bash
OLD_KEY_ID="key_abc123"  # replace with actual old key ID

curl -s -X DELETE "$BASE_URL/v1/auth/keys/$OLD_KEY_ID" \
  -H "Authorization: Bearer $NEW_KEY" | jq .
```

After this, the old key will return `401` on all requests. Update any systems that use it to the new key.

---

## Error Handling and Recovery

### `401 Unauthorized`

Your API key is wrong, revoked, or missing.

1. Verify the `Authorization: Bearer <key>` header is present and formatted correctly.
2. Check whether the key was recently rotated or deleted: `GET /v1/auth/keys`
3. If the key is gone, you need a new one — which requires a working key to create. If you have lost all keys, you need the bootstrap token to register a new tenant.

### `429 Too Many Requests`

You have hit a rate limit. The response body will indicate whether it is tenant registration (5/IP/hour, 20 global/hour) or another endpoint.

1. Read the `Retry-After` header if present — wait that many seconds.
2. If no `Retry-After`, back off exponentially: wait 60s, then 120s, then 240s.
3. Do not hammer the endpoint in a loop — this makes the problem worse.

### Service Stuck in `deploying`

If a service stays in `deploying` for more than 3 minutes:

1. Check detailed health to confirm Docker and gVisor are operational:
   ```bash
   curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/system/health/detailed" | jq .
   ```
2. Check whether the image exists and is pullable. If it is a private registry image, the server may not have credentials.
3. Try stopping and starting the service:
   ```bash
   curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/stop" \
     -H "Authorization: Bearer $API_KEY"
   sleep 5
   curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/start" \
     -H "Authorization: Bearer $API_KEY"
   ```
4. If it still does not move, check the ah daemon logs on the server: `journalctl -u ah -n 100 --no-pager`

### Circuit Breaker Open (`circuit_open` status)

The circuit breaker opens when a service crashes 5 times within 10 minutes. This prevents a crash-looping container from consuming resources.

1. Investigate why it crashed. Check recent logs if your service emits them.
2. Fix the underlying issue — bad config, missing env var, OOM, etc.
3. Once fixed (e.g., after updating env vars or deploying a new image), reset the circuit breaker:
   ```bash
   curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/reset" \
     -H "Authorization: Bearer $API_KEY" | jq .
   ```
4. Start the service:
   ```bash
   curl -s -X POST "$BASE_URL/v1/services/$SERVICE_ID/start" \
     -H "Authorization: Bearer $API_KEY" | jq .
   ```
5. Watch status for the next few minutes to confirm it stays `running`.

Do NOT keep resetting the circuit breaker without fixing the root cause. The circuit breaker is protecting the server.

### Build Failed

1. Get the build logs immediately:
   ```bash
   curl -s -H "Authorization: Bearer $API_KEY" \
     "$BASE_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs"
   ```
2. Common causes and fixes:
   - **"repository not found" / "authentication required"**: The Git URL is wrong or the repo is private. Use an HTTPS URL with credentials embedded (`https://token@github.com/owner/repo`) or make the repo public.
   - **"unable to detect language"**: Nixpacks could not identify the project type. Add a `nixpacks.toml` to the repo root to specify the build plan.
   - **Build timeout**: The build took too long. Check if there are unnecessary large dependencies. Simplify the build.
   - **"no such file or directory"**: Your `Procfile` or start command references a file that does not exist in the built output.
3. After fixing the issue, cancel any stuck build and start a new one:
   ```bash
   curl -s -X DELETE "$BASE_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID" \
     -H "Authorization: Bearer $API_KEY"
   ```
   Then re-trigger with `POST /v1/services/{id}/builds`.

### Database Creation Timeout (up to 30s)

Database provisioning is slow by design. If your HTTP client times out:

1. Wait 30 seconds.
2. Check if the database was created anyway:
   ```bash
   curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/databases" | jq .
   ```
3. If it exists, use it. Do not create a duplicate.
4. If it does not exist, retry with the SAME idempotency key to safely retry:
   ```bash
   curl -s -X POST "$BASE_URL/v1/databases" \
     -H "Authorization: Bearer $API_KEY" \
     -H "Content-Type: application/json" \
     -H "Idempotency-Key: $IDEMPOTENCY_KEY" \
     -d '{"name": "my-app-db", "type": "postgres"}' | jq .
   ```

### Disk Full (90% Watermark)

When disk usage exceeds 90%, ah rejects new deployments. You will receive an error indicating the disk threshold has been reached.

1. Check current disk status:
   ```bash
   curl -s -H "Authorization: Bearer $API_KEY" \
     "$BASE_URL/v1/system/health/detailed" | jq '.disk'
   ```
2. Free up space on the server. Options:
   - Remove old Docker images: `ssh root@server "docker image prune -af"`
   - Delete unused build artifacts
   - Remove stopped containers: `ssh root@server "docker container prune -f"`
3. Do NOT delete running service volumes without checking what data is stored there.
4. After freeing space, retry the deployment.

---

## Operational Checks

Run these checks daily (or at the start of any session) to confirm the platform is healthy before doing any work.

### 1 — Basic Health Check

```bash
curl -s "$BASE_URL/v1/system/health" | jq .
```

Expected: `{"status": "ok"}`. If not OK, stop and investigate before proceeding.

### 2 — Detailed Health (Disk, Docker, gVisor)

```bash
curl -s -H "Authorization: Bearer $API_KEY" \
  "$BASE_URL/v1/system/health/detailed" | jq .
```

Check:
- `disk.used_percent` — alert if above 75%, act if above 90%
- `docker.status` — must be `ok`
- `gvisor.status` — must be `ok` (gVisor provides sandbox isolation for containers)

If Docker or gVisor is not OK, new deployments will fail and existing services may be degraded.

### 3 — Check for Services in `circuit_open` State

```bash
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/services" | \
  jq '[.[] | select(.status == "circuit_open")]'
```

Any results here mean a service is crash-looping. Investigate and fix before the service owner notices.

### 4 — Check for Failed or Stuck Builds

```bash
# List all services first
SERVICES=$(curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/services" | jq -r '.[].id')

# For each service, check for failed builds in the last 24h
for SVC_ID in $SERVICES; do
  curl -s -H "Authorization: Bearer $API_KEY" \
    "$BASE_URL/v1/services/$SVC_ID/builds" | \
    jq --arg svc "$SVC_ID" '[.[] | select(.status == "failed" or .status == "building") | {service: $svc, build: .id, status: .status, started: .created_at}]'
done
```

Builds stuck in `building` for more than 15 minutes should be cancelled and restarted.

### 5 — List All Resources (Sanity Check)

```bash
echo "=== SERVICES ==="
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/services" | jq '[.[] | {id, name, status, url}]'

echo "=== DATABASES ==="
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/databases" | jq '[.[] | {id, name, type, status}]'

echo "=== API KEYS ==="
curl -s -H "Authorization: Bearer $API_KEY" "$BASE_URL/v1/auth/keys" | jq '[.[] | {id, name, created_at}]'
```

---

## Background Processes (for awareness)

You do not control these directly, but understanding them helps you reason about system behavior.

| Process | Interval | What it does |
|---------|----------|--------------|
| **Reconciler** | Every 60 seconds | Scans all services, ensures desired state matches actual state. If a service should be running but its container is gone, the reconciler restarts it. |
| **Garbage Collector** | Every 5 minutes | Cleans up old Docker images, stopped containers, and other stale resources. If you delete a service, the GC eventually removes its underlying container and image. |
| **Circuit Breaker** | Continuous | Monitors crash events per service. After 5 crashes in a 10-minute window, it opens the circuit and stops the service. You must call `/reset` and then `/start` to recover. |
| **Disk Watchdog** | Continuous | Monitors disk usage. Above 90%, new deploys are rejected. The reconciler will not start new containers until disk is below the threshold. |

The reconciler means that manually stopping a Docker container outside the API will cause it to be restarted within 60 seconds. Always use the API to manage service state.

---

## Important Limits

| Limit | Value |
|-------|-------|
| Max databases per tenant | 3 |
| Max API keys per tenant | 20 |
| Tenant registration rate limit (per IP) | 5 per hour |
| Tenant registration rate limit (global) | 20 per hour |
| Circuit breaker threshold | 5 crashes in 10 minutes |
| Disk watermark (blocks new deploys) | 90% used |
| Database creation timeout | Up to 30 seconds |
| Build sources supported | GitHub, GitLab, Bitbucket, sr.ht, Codeberg (HTTPS only) |
| Idempotency key scope | POST, PUT, DELETE requests |
| Service registration header | `X-Bootstrap-Token` (HMAC-compared) |

---

## Notes for AI Agents

- **Never guess at service or database IDs.** Always list first, then operate on specific IDs.
- **Always use idempotency keys** for database creation and build triggers — these are the two operations most likely to be retried after a network timeout.
- **Poll, do not assume.** Deployments and builds are async. Check status before declaring success.
- **Read before you write.** Before setting env vars, list existing ones (`GET /v1/services/{id}/env`) so you do not accidentally overwrite values set by others.
- **The circuit breaker protects the server.** If a service keeps crashing, do not just reset the circuit breaker in a loop. Fix the root cause first.
- **One API key per agent.** Create a named key for yourself so it can be revoked independently if needed.
