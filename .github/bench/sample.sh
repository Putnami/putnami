#!/usr/bin/env bash
# Samples the process table every 5s and dumps the env and argv of go test processes once.
out="$1"; mkdir -p "$out"; dumped=0
while true; do
  { echo "=== $(date +%s) procs=$(ls -d /proc/[0-9]* | wc -l)"; ps -eo pid,ppid,pcpu,etimes,rss,comm,args --sort=-pcpu | head -30 | cut -c1-400; } >> "$out/ps.txt"
  if [ $dumped -lt 3 ]; then
    for p in $(pgrep -f '\.test( |$)|go test|/go-build.*\.test' | head -3); do
      { echo "--- pid $p"; tr '\0' ' ' < /proc/$p/cmdline; echo; tr '\0' '\n' < /proc/$p/environ | sort; } >> "$out/env.txt" 2>/dev/null
    done
    [ -s "$out/env.txt" ] && dumped=$((dumped+1))
  fi
  sleep 5
done
