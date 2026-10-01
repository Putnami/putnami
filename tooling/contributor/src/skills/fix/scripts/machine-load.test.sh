#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$ROOT/.agents/skills/fix/scripts/machine-load.sh"

# The real machine answers with a non-negative ratio with two decimals.
status=0
ratio="$(bash "$SCRIPT" 2>&1)" || status=$?
if [ "$status" != 0 ]; then
  echo "machine-load test: the real machine reported no load ratio (exit $status): $ratio; PATH=$PATH; /proc/loadavg $([ -r /proc/loadavg ] && echo readable || echo absent); $(command -v getconf nproc sysctl awk | tr '\n' ' ')" >&2
  exit 1
fi
if ! [[ "$ratio" =~ ^[0-9]+\.[0-9]{2}$ ]]; then
  echo "machine-load test: expected a ratio with two decimals, got '$ratio'" >&2
  exit 1
fi

# A platform that reports no CPU count prints nothing and exits 2.
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_DIR/bin" "$TEST_DIR/proc"
printf '0.50 0.40 0.30 1/100 1\n' >"$TEST_DIR/proc/loadavg"
for tool in getconf nproc sysctl; do
  printf '#!/bin/sh\nexit 1\n' >"$TEST_DIR/bin/$tool"
  chmod +x "$TEST_DIR/bin/$tool"
done
status=0
output="$(MACHINE_LOAD_PROC="$TEST_DIR/proc" PATH="$TEST_DIR/bin:$PATH" bash "$SCRIPT")" || status=$?
if [ "$status" != 2 ] || [ -n "$output" ]; then
  echo "machine-load test: expected exit 2 and no output without a CPU count, got $status '$output'" >&2
  exit 1
fi

echo "machine-load test: ok"
