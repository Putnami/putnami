#!/usr/bin/env bash
set -uo pipefail
B="$(cd "$(dirname "$0")" && pwd)"; R=$B/results; W=/home/user; C=$W/wt-bench-combined
# wait for campaign2 (ablation) to finish
while pgrep -f campaign2.sh >/dev/null; do sleep 5; done
$B/run.sh --putnami $W/wt-bench-main --putnami $C --putnami $C@BENCH_DB=tx --putnami $C@BENCH_DB=txa \
  --putnami $C@BENCH_DB=raw --putnami $C@BUN_OPTIONS=--smol --elysia --bun --routes health,get --rounds 3 \
  --out $R/nosmol-main > $R/nosmol-main.out 2>&1
$B/profiles.sh
echo campaign3 done
