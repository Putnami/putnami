#!/usr/bin/env bash
# Print this machine's load as a ratio of its capacity: the one-minute load
# average divided by the number of online logical CPUs, with two decimals.
# 1.00 means every CPU is busy. Exit 2, printing nothing, when the platform
# reports neither value; the caller then gates locally.
set -euo pipefail

# MACHINE_LOAD_PROC points at another proc root; the test uses a fake one.
PROC="${MACHINE_LOAD_PROC:-/proc}"

load=""
cpus=""
if [ -r "$PROC/loadavg" ]; then
  read -r load _ <"$PROC/loadavg"
elif command -v sysctl >/dev/null 2>&1; then
  # macOS and the BSDs print "{ 1.23 1.10 0.98 }".
  load="$(sysctl -n vm.loadavg 2>/dev/null | tr -d '{}' | awk '{print $1}' || true)"
fi
if command -v getconf >/dev/null 2>&1; then
  cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || true)"
fi
if [ -z "$cpus" ] && command -v nproc >/dev/null 2>&1; then
  cpus="$(nproc 2>/dev/null || true)"
fi
if [ -z "$cpus" ] && [ -r "$PROC/cpuinfo" ]; then
  cpus="$(awk '/^processor[[:space:]]*:/ { n++ } END { if (n) print n }' "$PROC/cpuinfo")"
fi
if [ -z "$cpus" ] && command -v sysctl >/dev/null 2>&1; then
  cpus="$(sysctl -n hw.logicalcpu 2>/dev/null || true)"
fi

case "$load" in '' | *[!0-9.]*) exit 2 ;; esac
case "$cpus" in '' | *[!0-9]* | 0) exit 2 ;; esac
# gawk reserves `load`, so the awk variables carry other names.
awk -v avg="$load" -v cpus="$cpus" 'BEGIN { printf "%.2f\n", avg / cpus }'
