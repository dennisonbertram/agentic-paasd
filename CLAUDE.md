# CLAUDE.md — agentic-hosting

## Project Overview

Single Go binary PaaS designed for AI agents: deploy, manage, and monitor services via HTTP API with full isolation via gVisor. Stack: Go 1.25, SQLite (WAL mode), Docker + gVisor (runsc), Nixpacks build pipeline, Traefik reverse proxy. See docs/implementation/build-log.md for the full system overview.

## Working Here

- Default working dir: `~/Develop/agentic-hosting` (capital D)
- Always read `docs/implementation/build-log.md` at the start of a new session to orient yourself
- Read `CHANGELOG.md` to understand recent changes
- Server: `65.21.67.254`, SSH: `ssh -i ~/.ssh/id_hetzner_claudeops root@65.21.67.254`

## Commit Conventions

- Format: `type(scope): short description` where type is `feat` / `fix` / `chore` / `docs` / `test` / `refactor`
- Always run `go build ./...` before committing
- Always run `go test ./...` before committing (once tests exist)
- Never commit `.db` files, `.key` files, or `.env` files
- Commit messages should explain WHY, not just WHAT

## Testing Rules

- All new packages must have a corresponding `_test.go` file
- Use testify for assertions (`github.com/stretchr/testify`)
- Use `internal/testutil` for shared test helpers (in-memory SQLite, mock Docker client)
- Run tests: `make test`
- Run with coverage: `make test-coverage`
- Target: no package should be merged with 0% coverage if it contains business logic

## Session Log Discipline

- At the END of every work session, append a summary to `docs/sessions/YYYY-MM-DD.md`
- Format: what was planned, what was done, what was left incomplete, any blockers
- This replaces `~/docs/paasd/log.md` (that was the old location)

## What to Never Do

- Never hardcode secrets, tokens, or keys
- Never push directly to main for significant changes (use a branch + PR)
- Never skip the session log entry
- Never mark a task done without verifying `go build ./...` passes
- Never modify the master key path or bootstrap token logic without a security review

## Ralph Loop

Use the Ralph Loop (iterative AI review) for:

- New API endpoints
- Security-sensitive changes (auth, crypto, rate limiting)
- Reconciler logic changes
- Any change touching circuit breaker or health probe logic

## Key Files

| Path | Purpose |
|---|---|
| `cmd/ah/main.go` | Entry point |
| `internal/api/` | All API handlers |
| `internal/reconciler/reconciler.go` | State reconciliation loop |
| `internal/docker/client.go` | Docker Engine API wrapper |
| `internal/db/migrations/` | DB migration files |
| `internal/testutil/` | Shared mocks and test helpers |
