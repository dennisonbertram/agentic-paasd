---
description: Run a security review of the agentic-hosting codebase. Checks for auth bypass, injection, secrets exposure, container escape, and API security issues.
allowed-tools: Bash
---

You are a security auditor specializing in Go backend services, container runtimes, and self-hosted PaaS infrastructure. Perform a thorough security review of the agentic-hosting codebase.

## Setup

The repo is at the current working directory (or find it with `find ~ -name "go.mod" -path "*/agentic-hosting/*" 2>/dev/null | head -1 | xargs dirname`).

Flatten the codebase:
```bash
cd <repo-root>
repomix --ignore "vendor/,*.sum,*.mod,repomix*" -o /tmp/ah-review.txt .
wc -l /tmp/ah-review.txt
```

## Review Areas

Pipe the flattened code to an LLM for each of these focused passes:

### Pass 1 — Authentication & Authorization
```bash
cat /tmp/ah-review.txt | llm -m gpt-4.1 "You are a security auditor. Review this Go codebase for authentication and authorization issues:

- Bootstrap token validation: is HMAC comparison timing-safe? Any bypass paths?
- API key validation: prefix-based lookup correct? Timing-safe comparison?
- Are there any endpoints missing auth middleware?
- Can a tenant access another tenant's resources?
- Is the master key used correctly for HMAC derivation?

List every issue as CRITICAL, HIGH, MEDIUM, or LOW with file:line references. Be specific."
```

### Pass 2 — Injection & Container Escape
```bash
cat /tmp/ah-review.txt | llm -m gpt-4.1 "You are a security auditor. Review this Go codebase for injection and container escape risks:

- Are user-supplied strings ever passed to shell commands (exec.Command with user input)?
- Are Docker API calls using user input safely (image names, env vars, labels)?
- gVisor sandbox: is runsc actually enforced? Can a container escape to the host?
- Are there path traversal risks in any file operations?
- SQL injection in SQLite queries (raw string interpolation vs prepared statements)?

List every issue as CRITICAL, HIGH, MEDIUM, or LOW with file:line references."
```

### Pass 3 — Secrets & Data Exposure
```bash
cat /tmp/ah-review.txt | llm -m gpt-4.1 "You are a security auditor. Review this Go codebase for secrets and data exposure:

- Are API keys or tokens ever logged?
- Is the master key ever exposed in error messages or API responses?
- Are database connection strings (with credentials) returned to clients safely?
- Is sensitive data encrypted at rest in SQLite?
- Are there any hardcoded secrets or credentials?
- Is TLS enforced for external communication?

List every issue as CRITICAL, HIGH, MEDIUM, or LOW with file:line references."
```

### Pass 4 — Denial of Service & Resource Limits
```bash
cat /tmp/ah-review.txt | llm -m gpt-4.1 "You are a security auditor. Review this Go codebase for DoS and resource exhaustion:

- Are there rate limits on any endpoints?
- Can a single tenant exhaust disk, CPU, or memory?
- Are there request body size limits?
- Can a malicious git URL or Docker image trigger unbounded resource usage during build?
- Are goroutines properly bounded/cancelled?
- Circuit breaker logic: can it be bypassed or exploited?

List every issue as CRITICAL, HIGH, MEDIUM, or LOW with file:line references."
```

## Output Format

After all passes, produce a consolidated report:

```
# agentic-hosting Security Review
Date: <today>

## Summary
- CRITICAL: N issues
- HIGH: N issues
- MEDIUM: N issues
- LOW: N issues

## Critical & High Issues
[List with file:line, description, and recommended fix]

## Medium Issues
[List with file:line and description]

## Low / Informational
[Brief list]

## Positive Findings
[What the codebase does well security-wise]
```

Save the report to `docs/investigations/security-review-<date>.md` in the repo.
