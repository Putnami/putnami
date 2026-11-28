#!/usr/bin/env bash
# Measurement campaign of REPORT.md (without BUN_OPTIONS=--smol); each run takes the lock on its own.
set -uo pipefail
B="$(cd "$(dirname "$0")" && pwd)"; R=$B/results; W=/home/user; C=$W/wt-bench-combined
$B/run.sh --putnami $W/wt-bench-main --putnami $C --putnami $C@BENCH_DB=tx --putnami $C@BENCH_DB=txa \
  --putnami $C@BENCH_DB=raw --putnami $C@BUN_OPTIONS=--smol --elysia --bun --routes health,get --rounds 3 \
  --out $R/nosmol-main > $R/nosmol-main.out 2>&1
$B/run.sh --putnami $C --putnami $C@BENCH_OFF=logger --putnami $C@BENCH_OFF=platform \
  --putnami $C@BENCH_OFF=securityHeaders --putnami $C@BENCH_OFF=originGuard --putnami $C@BENCH_OFF=deadline \
  --putnami $C@BENCH_OFF=httpMethods --putnami "$C@BENCH_OFF=logger+platform+securityHeaders+originGuard+deadline+httpMethods" \
  --elysia --routes health --rounds 3 --no-parity --out $R/ablation > $R/ablation.out 2>&1
echo campaign2 done
