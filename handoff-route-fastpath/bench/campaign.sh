#!/usr/bin/env bash
# The measurement campaign of REPORT.md; each run takes the lock on its own.
set -uo pipefail
B="$(cd "$(dirname "$0")" && pwd)"; R=$B/results; W=/home/user
$B/run.sh --putnami $W/wt-bench-main --putnami $W/wt-bench-logger --putnami $W/wt-bench-awaits \
  --putnami $W/wt-bench-combined --elysia --bun --routes health,get --rounds 3 --out $R/branches > $R/branches.out 2>&1
$B/run.sh --putnami $W/wt-bench-combined --putnami $W/wt-bench-combined@BENCH_DB=tx \
  --putnami $W/wt-bench-combined@BENCH_DB=raw --elysia --routes get,list --rounds 3 --out $R/dbab > $R/dbab.out 2>&1
C=$W/wt-bench-combined
$B/run.sh --putnami $C --putnami $C@BENCH_OFF=logger --putnami $C@BENCH_OFF=platform \
  --putnami $C@BENCH_OFF=securityHeaders --putnami $C@BENCH_OFF=originGuard --putnami $C@BENCH_OFF=deadline \
  --putnami $C@BENCH_OFF=httpMethods --putnami "$C@BENCH_OFF=logger+platform+securityHeaders+originGuard+deadline+httpMethods" \
  --routes health --rounds 3 --no-parity --out $R/ablation > $R/ablation.out 2>&1
$B/run.sh --putnami $W/wt-bench-main --putnami $C --elysia --bun --routes list,insert --rounds 3 --out $R/list-insert > $R/list-insert.out 2>&1
echo campaign done
