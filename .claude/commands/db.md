---
description: Provision a Postgres or Redis database and wire it to a service. Usage: /db <service-name> <postgres|redis> [db-name]
argument-hint: <service-name> <postgres|redis> [db-name]
allowed-tools: Bash
---

Provision a `$2` database named `${3:-$1-db}` and wire it to service `$1` on agentic-hosting.

You are an agentic-hosting operator using `$AH_URL` and `$AH_KEY`.

Steps:
1. Find the service by name: `GET /v1/services` — filter by name == `$1`, get its ID. If not found, tell the user.
2. Create the database with an idempotency key (database creation can take up to 30s — be patient):
   ```
   POST /v1/databases {"name": "${3:-$1-db}", "type": "$2"}
   Idempotency-Key: <uuid>
   ```
3. Poll for database `running` status every 5 seconds, up to 60 seconds
4. Get the connection string: `GET /v1/databases/{db-id}/connection-string`
5. Set the appropriate env var on the service:
   - postgres → `DATABASE_URL`
   - redis → `REDIS_URL`
6. Restart the service: `POST /v1/services/{service-id}/restart`
7. Poll for service `running` status

Report: database ID, connection string (masked — show only the host:port part), service restart status.

**Never print the full connection string** — it contains credentials. Only show host:port.
