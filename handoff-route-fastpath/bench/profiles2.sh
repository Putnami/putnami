#!/usr/bin/env bash
# Remaining profiles in one lock hold (about 2 minutes).
set -uo pipefail
B="$(cd "$(dirname "$0")" && pwd)"; P=$B/profiles
uptime
python3 -I $B/profile.py --elysia --route health --out $P/elysia-health > $P/elysia-health.out 2>&1
python3 -I $B/profile.py --elysia --route get --out $P/elysia-get > $P/elysia-get.out 2>&1
python3 -I $B/profile.py --putnami /home/user/wt-bench-combined@BENCH_DB=raw --route get --out $P/combined-raw-get > $P/combined-raw-get.out 2>&1
python3 -I $B/profile.py --putnami /home/user/wt-bench-combined@BENCH_DB=raw --route health --out $P/combined-raw-health > $P/combined-raw-health.out 2>&1
uptime
