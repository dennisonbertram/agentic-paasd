#!/usr/bin/env bash
set -euo pipefail

URL="${AH_URL:?Set AH_URL}"
KEY="${AH_KEY:?Set AH_KEY}"

# System health
echo "=== System Health ==="
HEALTH=$(curl -sf -H "Authorization: Bearer $KEY" "$URL/v1/system/health/detailed")
echo "$HEALTH" | python3 -c "
import json,sys
h = json.load(sys.stdin)
print(f\"  Status : {h.get('status','?')}\")
disk = h.get('disk', {})
used_pct = disk.get('used_percent', 0)
warn = ' WARNING' if used_pct > 80 else ''
alert = ' CRITICAL' if used_pct > 90 else ''
print(f\"  Disk   : {used_pct:.1f}%{warn}{alert}\")
docker = h.get('docker', {})
print(f\"  Docker : {docker.get('version','?')}\")
gvisor = h.get('gvisor', {})
print(f\"  gVisor : {'ok' if gvisor.get('available') else 'NOT AVAILABLE'}\")
" 2>/dev/null || echo "$HEALTH"

echo ""
echo "=== Services ==="
SVCS=$(curl -sf -H "Authorization: Bearer $KEY" "$URL/v1/services")
echo "$SVCS" | python3 -c "
import json,sys
svcs = json.load(sys.stdin)
if not svcs:
    print('  (none)')
    sys.exit(0)
for s in svcs:
    status = s['status']
    icon = 'OK' if status == 'running' else 'FAIL'
    cb = ' [CIRCUIT OPEN]' if s.get('circuit_open') else ''
    crashes = f\" crashes={s['crash_count']}\" if s.get('crash_count', 0) > 0 else ''
    print(f\"  [{icon}] {s['name']:<20} {status}{cb}{crashes}\")
" 2>/dev/null || echo "$SVCS"

echo ""
echo "=== Databases ==="
DBS=$(curl -sf -H "Authorization: Bearer $KEY" "$URL/v1/databases")
echo "$DBS" | python3 -c "
import json,sys
dbs = json.load(sys.stdin)
if not dbs:
    print('  (none)')
    sys.exit(0)
for d in dbs:
    status = d['status']
    icon = 'OK' if status == 'running' else 'FAIL'
    print(f\"  [{icon}] {d['name']:<20} {d['type']:<10} {status}\")
" 2>/dev/null || echo "$DBS"
