# agentic-paasd

A self-hosted PaaS for bare metal servers, designed to be operated by AI agents via REST API. No web dashboard. Agents are the operators.

- **Repo**: github.com/dennisonbertram/agentic-paasd
- **Stack**: Go binary + Docker + gVisor + Traefik + Nixpacks + SQLite
- **Language**: Go 1.25 (requires CGO for SQLite)
- **Binary**: `paasd`
- **Default port**: 8080 (loopback only, behind Traefik)

## Design Principles

1. **Agentic-first** — No web dashboard. AI agents use the REST API directly.
2. **One happy path per primitive** — One database engine per type (Postgres, Redis), one build system (Nixpacks), one router (Traefik).
3. **Multi-tenant** — Full tenant isolation via API keys, namespaced resources, per-tenant rate limits.
4. **Secure by default** — gVisor syscall interception, ReadonlyRootfs, dropped capabilities, AES-256-GCM encrypted secrets.

## Architecture

```
                    Internet
                       │
                    Traefik (80/443)
                       │
        ┌──────────────┼──────────────┐
        │              │              │
   paasd API      Service A      Service B
   (127.0.0.1:8080)  (gVisor)       (gVisor)
        │
   SQLite DBs
   /var/lib/paasd/
```

- **paasd** — control plane (single Go binary, systemd service)
- **Traefik** — reverse proxy, discovers services via Docker labels, handles TLS
- **gVisor (runsc)** — container runtime, syscall interception for tenant isolation
- **Nixpacks** — zero-config source-to-image builds
- **SQLite** — two databases: `paasd.db` (state) and `paasd-metering.db` (metering), WAL mode

## Server Requirements

- Linux (Ubuntu 22.04+ recommended)
- Docker 29+ with gVisor runtime configured
- Go 1.21+ with CGO enabled
- Nixpacks installed
- Traefik running as Docker container
- `PAASD_BOOTSTRAP_TOKEN` environment variable (min 32 chars)
- `/var/lib/paasd/master.key` — hex-encoded 32-byte key

## Installation

### 1. Install dependencies

```bash
# Docker
curl -fsSL https://get.docker.com | sh

# gVisor
curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list
sudo apt-get update && sudo apt-get install -y runsc

# Configure Docker to use gVisor
cat > /etc/docker/daemon.json <<'EOF'
{
  "runtimes": {
    "runsc": {
      "path": "/usr/bin/runsc"
    }
  },
  "default-address-pools": [{"base":"172.20.0.0/14","size":24}]
}
EOF
systemctl restart docker

# Go
wget -q https://go.dev/dl/go1.25.0.linux-amd64.tar.gz -O /tmp/go.tar.gz
tar -C /usr/local -xzf /tmp/go.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile

# Nixpacks
curl -sSL https://nixpacks.com/install.sh | bash
```

### 2. Start infrastructure

```bash
# Create Docker network
docker network create --driver bridge traefik-public

# Start Traefik
docker run -d --name paas-traefik --restart unless-stopped \
  --network traefik-public \
  -p 80:80 -p 443:443 -p 8090:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /etc/traefik:/etc/traefik \
  traefik:latest \
  --api.dashboard=true \
  --providers.docker=true \
  --providers.docker.network=traefik-public \
  --providers.file.filename=/etc/traefik/dynamic/paasd.yml \
  --entrypoints.web.address=:80 \
  --entrypoints.websecure.address=:443

# Start local registry
docker run -d --name paas-registry --restart unless-stopped \
  -p 127.0.0.1:5000:5000 \
  -v /var/lib/paasd/registry:/var/lib/registry \
  registry:2
```

### 3. Build and install paasd

```bash
# Clone repo
git clone https://github.com/dennisonbertram/agentic-paasd /agentic-paasd
cd /agentic-paasd

# Build (CGO required for SQLite)
CGO_ENABLED=1 go build -o bin/paasd ./cmd/paasd
cp bin/paasd /usr/local/bin/paasd

# Create data directory
mkdir -p /var/lib/paasd/builds /var/lib/paasd/backups

# Generate master key (hex-encoded 32 bytes)
head -c 32 /dev/urandom | xxd -p -c 64 > /var/lib/paasd/master.key
chmod 600 /var/lib/paasd/master.key

# Generate bootstrap token
openssl rand -hex 32
```

### 4. Configure and start systemd service

```bash
# Set bootstrap token
cat > /etc/default/paasd <<'EOF'
PAASD_BOOTSTRAP_TOKEN=<your-32+-char-token-here>
EOF
chmod 600 /etc/default/paasd

# Install service
cp deploy/paasd.service /etc/systemd/system/paasd.service
systemctl daemon-reload
systemctl enable paasd
systemctl start paasd

# Verify
curl http://localhost:8080/v1/system/health
# → {"status":"ok"}
```

## API Reference

### Authentication

All endpoints except `GET /v1/system/health` and `POST /v1/tenants/register` require:

```
Authorization: Bearer <api_key>
```

API key format: `keyID.secret` (returned on registration and key creation).

### Registration

```bash
# Register a tenant (requires bootstrap token)
curl -X POST http://localhost:8080/v1/tenants/register \
  -H 'Content-Type: application/json' \
  -H 'X-Bootstrap-Token: <bootstrap-token>' \
  -d '{"name": "my-tenant", "email": "admin@example.com"}'

# Response
{
  "tenant_id": "ten_abc123",
  "api_key": "keyid.secret"
}
```

### Health

```bash
GET /v1/system/health           # Public — {"status":"ok"}
GET /v1/system/health/detailed  # Auth required — disk, docker, gVisor status
```

### Tenant Management

```bash
GET    /v1/tenant   # Get tenant info
PATCH  /v1/tenant   # Update name: {"name": "new-name"}
DELETE /v1/tenant   # Soft-delete (suspends tenant, stops all services)
```

### API Keys

```bash
POST   /v1/auth/keys             # Create key: {"name":"ci-key","expires_in":2592000}
GET    /v1/auth/keys             # List keys (secrets not shown)
DELETE /v1/auth/keys/{keyID}     # Revoke key
```

### Services

Services are Docker containers run with gVisor isolation.

```bash
# Create and deploy a service
POST /v1/services
{
  "name": "my-app",
  "image": "127.0.0.1:5000/tenant/my-app:latest",
  "port": 3000,
  "env": {"NODE_ENV": "production"},
  "memory_mb": 512,
  "cpu_count": 1
}
# Returns immediately with status "deploying"

GET    /v1/services                              # List services
GET    /v1/services/{serviceID}                  # Get service (includes status, URL)
DELETE /v1/services/{serviceID}                  # Delete service and container

POST   /v1/services/{serviceID}/start            # Start
POST   /v1/services/{serviceID}/stop             # Stop
POST   /v1/services/{serviceID}/restart          # Restart
POST   /v1/services/{serviceID}/reset            # Reset circuit breaker

GET    /v1/services/{serviceID}/env              # List env vars (masked by default)
GET    /v1/services/{serviceID}/env?reveal=true  # Show values
POST   /v1/services/{serviceID}/env              # Set vars: {"KEY": "value"}
DELETE /v1/services/{serviceID}/env/{key}        # Delete one var
```

### Builds (Nixpacks)

Build from a git repository. Supports GitHub, GitLab, Bitbucket, sr.ht, Codeberg (HTTPS only).

```bash
# Start a build
POST /v1/services/{serviceID}/builds
{
  "git_url": "https://github.com/org/repo",
  "branch": "main"
}
# Returns: {"build_id": "...", "status": "building", "image": "127.0.0.1:5000/..."}

GET    /v1/services/{serviceID}/builds                          # List builds
GET    /v1/services/{serviceID}/builds/{buildID}                # Get build status
GET    /v1/services/{serviceID}/builds/{buildID}/logs?follow=true  # Stream logs
DELETE /v1/services/{serviceID}/builds/{buildID}                # Cancel build
```

### Databases

Managed Postgres and Redis containers with encrypted connection strings.

```bash
# Create database (takes ~10-30s)
POST /v1/databases
{
  "name": "my-db",
  "type": "postgres"   # or "redis"
}

GET    /v1/databases                          # List databases
GET    /v1/databases/{dbID}                   # Get database
GET    /v1/databases/{dbID}/connection-string # Get connection string (decrypted)
DELETE /v1/databases/{dbID}                   # Delete database and data
```

## Operational Details

### File Layout

```
/var/lib/paasd/
├── paasd.db             # State database (tenants, services, builds, databases)
├── paasd-metering.db    # Metering database
├── master.key           # AES-256 master key (hex-encoded, chmod 600)
├── builds/              # Nixpacks build workdirs (GC after build)
├── backups/             # SQLite backups
└── registry/            # Docker registry data
```

### Background Processes

- **Reconciler** (60s interval): Syncs DB state to Docker reality. Detects crashes, circuit breakers.
- **GC** (5min interval): Cleans orphaned containers, volumes, images, build directories.
- **Circuit breaker**: 5 crashes in 10 minutes stops restarting. Reset with `POST /v1/services/{id}/reset`.
- **Disk watermarks**: Warns at 80% usage, blocks new deploys at 90%.

### Backups

```bash
paasd backup
# Creates timestamped gzip backups in /var/lib/paasd/backups/
# Keeps last 10 backups
# Uses VACUUM INTO for WAL-safe snapshots
```

### Security Model

- **gVisor**: All tenant containers run with `--runtime=runsc` (syscall interception)
- **ReadonlyRootfs**: Container root filesystem is read-only; tmpfs mounts for `/tmp`, `/var/run`, `/var/tmp`, `/run`
- **CapDrop=ALL**: No Linux capabilities
- **no-new-privileges**: Containers cannot escalate privileges
- **Encrypted secrets**: Env vars and database credentials encrypted with AES-256-GCM using tenant-derived keys
- **API keys**: bcrypt-hashed, prefix-indexed for performance
- **Rate limiting**: Per-tenant (100 req/s burst 200) + global (500 req/s burst 1000)
- **Idempotency**: POST/PUT/DELETE accept `Idempotency-Key` header (stored per tenant+method+path)

### Networking

- Services are accessible via Traefik at `http://<service-name>.<tenant-subdomain>.<domain>`
- Traefik discovers services via Docker labels set by paasd
- paasd API is loopback-only by default (Traefik reverse proxies it)

## Development

```bash
# Build
make build

# Run locally (requires Docker + gVisor)
PAASD_BOOTSTRAP_TOKEN=dev-token-32-chars-minimum make run

# Run in dev mode (no HTTPS enforcement, open registration)
./bin/paasd --dev --open-registration --port 8080

# Backup databases
./bin/paasd backup
```

## Claude Code Integration

paasd ships with a Claude Code skill, slash commands, and bash scripts for operating the API without writing curl commands manually.

### Install the Skill

Copy the skill to your Claude Code skills directory:

```bash
mkdir -p ~/.claude/skills/paasd
cp .claude/skills/paasd/SKILL.md ~/.claude/skills/paasd/SKILL.md
```

Claude will automatically load the skill and know how to operate paasd: authentication, deployment flows, database provisioning, error handling, and all API limits.

### Slash Commands

Copy the commands to your project:

```bash
cp -r .claude/commands/ /your/project/.claude/commands/
```

| Command | Description |
|---------|-------------|
| `/paasd-deploy <git-url-or-image> <name> [port]` | Deploy from a git URL (Nixpacks build) or Docker image |
| `/paasd-status` | Full dashboard — disk, services, databases, circuit breakers |
| `/paasd-db <service> <postgres\|redis> [name]` | Provision a database and wire it to a service |
| `/paasd-logs <service> [build-id]` | Stream build logs for a service |

**Example usage in Claude Code:**
```
/paasd-deploy https://github.com/org/my-app web 3000
/paasd-status
/paasd-db web postgres
/paasd-logs web
```

### Bash Scripts

Standalone scripts that work without Claude:

```bash
# Set credentials
export PAASD_URL="https://<your-domain>"
export PAASD_KEY="keyid.secret"

# Register a new tenant (one-time)
PAASD_BOOTSTRAP_TOKEN=<token> ./scripts/register.sh my-tenant me@example.com

# Deploy from git or Docker image
./scripts/deploy.sh https://github.com/org/repo my-app 3000
./scripts/deploy.sh nginx:alpine my-site 80

# Check status of everything
./scripts/status.sh

# Provision a database and wire it to a service
./scripts/db-provision.sh my-app postgres
./scripts/db-provision.sh my-app redis

# Stream build logs
./scripts/logs.sh my-app
```

See [`scripts/README.md`](scripts/README.md) for full documentation.

## Planned Work

See [GitHub Issues](https://github.com/dennisonbertram/agentic-paasd/issues) for planned enhancements including:

- HKDF key separation
- Multi-domain routing
- Billing/metering API
- mTLS for inter-service communication
- Audit logging
