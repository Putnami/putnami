#!/usr/bin/env bash
# The CPU profiles of REPORT.md; each takes the lock on its own.
set -uo pipefail
B="$(cd "$(dirname "$0")" && pwd)"; P=$B/profiles
for r in health get; do
  $B/prof.sh --putnami /home/user/wt-bench-combined --route $r --out $P/combined-$r > $P/combined-$r.out 2>&1
  $B/prof.sh --elysia --route $r --out $P/elysia-$r > $P/elysia-$r.out 2>&1
done
$B/prof.sh --putnami /home/user/wt-bench-combined@BENCH_DB=raw --route get --out $P/combined-raw-get > $P/combined-raw-get.out 2>&1
echo profiles done
