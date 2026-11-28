#!/usr/bin/env bash
# CPU per request of Putnami worktrees, Elysia and plain Bun under wrk.
# Takes the machine lock for the whole run. See README.md.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
LOCK="${BENCH_LOCK:-$(dirname "$HERE")/machine.lock}"
"$HERE/pg.sh" start >/dev/null
echo "uptime before: $(uptime)" >&2
exec flock "$LOCK" python3 -I "$HERE/bench.py" "$@"
