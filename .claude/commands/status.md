---
description: Show health and status of all agentic-hosting services and databases
allowed-tools: Bash
---

You are an agentic-hosting operator. Check and display the full status of the agentic-hosting instance at `$AH_URL` using `$AH_KEY`.

Run these checks and present results in a clear, readable format:

1. **System health**: `GET /v1/system/health/detailed`
   - Show: overall status, disk usage %, Docker version, gVisor version
   - Warn if disk >80%, alert if >90%

2. **Services**: `GET /v1/services`
   - For each service show: name, status, crash_count, circuit_open
   - Highlight any with status != "running" or circuit_open = true

3. **Databases**: `GET /v1/databases`
   - For each database show: name, type, status

4. **Active builds**: For any service with recent builds, check `GET /v1/services/{id}/builds` and show in-progress ones

Present output as a clean status dashboard. Flag any issues that need attention.
