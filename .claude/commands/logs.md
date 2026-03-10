---
description: Stream build logs for a service. Usage: /logs <service-name> [build-id]
argument-hint: <service-name> [build-id]
allowed-tools: Bash
---

Stream logs for service `$1` on agentic-hosting at `$AH_URL` using `$AH_KEY`.

You are an agentic-hosting operator.

Steps:
1. Find the service by name: `GET /v1/services` — filter by name == `$1`
2. If `$2` is provided, use that as the build ID. Otherwise, get the latest build: `GET /v1/services/{id}/builds` — use the first result
3. Show build metadata: status, git_url, branch, started_at
4. Stream logs: `GET /v1/services/{id}/builds/{build-id}/logs?follow=true`
   - Use curl with `-N` flag for streaming
5. After logs complete, show final build status

**Note**: Runtime container logs (stdout/stderr) are not yet available via API (see GitHub issue #11). Only build logs are supported.

If no builds exist for the service, tell the user and suggest starting one with `/deploy`.
