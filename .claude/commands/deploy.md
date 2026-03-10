---
description: Deploy a service to agentic-hosting from a git URL or Docker image. Usage: /deploy <git-url-or-image> <service-name> [port]
argument-hint: <git-url-or-image> <service-name> [port]
allowed-tools: Bash
---

Deploy `$2` to agentic-hosting using `$1` on port `${3:-3000}`.

You are an agentic-hosting operator. Use `$PAASD_URL` and `$PAASD_KEY` from the environment (ask the user if not set).

Follow this exact flow:

1. Check connectivity: `curl -s $PAASD_URL/v1/system/health`
2. Check disk: `curl -s -H "Authorization: Bearer $PAASD_KEY" $PAASD_URL/v1/system/health/detailed` — abort if disk >90%
3. Determine if `$1` is a git URL (starts with https://) or a Docker image name
4. **If Docker image**: Create service directly with the image
5. **If git URL**: 
   - Create service with placeholder image first
   - Start a Nixpacks build with the git URL
   - Stream build logs until complete
   - The build auto-deploys on success
6. Poll for `running` status every 5 seconds, up to 10 minutes
7. Report the final service URL and status

Use `Idempotency-Key: $(uuidgen)` on the POST /v1/services call.

Always show the user:
- Service ID
- Build ID (if applicable)
- Final status
- URL (from the service response)
- Any errors with clear explanation
