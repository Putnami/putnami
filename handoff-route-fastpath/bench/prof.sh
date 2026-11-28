#!/usr/bin/env bash
# CPU profile of one subject under wrk, under the machine lock. See README.md.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
LOCK="${BENCH_LOCK:-$(dirname "$HERE")/machine.lock}"
"$HERE/pg.sh" start >/dev/null
echo "uptime before: $(uptime)" >&2
exec flock "$LOCK" python3 -I "$HERE/profile.py" "$@"
