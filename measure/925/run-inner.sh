#!/usr/bin/env bash
# Runs as PID 1 of a fresh PID namespace (started by harness.py).
# BSD process accounting switched on here records only the processes of this
# namespace, so every process of the run, detached ones included, is counted,
# and nothing else on the machine is.
#
# Usage: run-inner.sh <worktree> <outdir> <putnami arguments...>
set -u
wt=$1; out=$2; shift 2
: > "$out/pacct"
accton "$out/pacct" >/dev/null
cd "$wt" || exit 90
start=$EPOCHREALTIME
./putnamiw "$@" > "$out/run.log" 2>&1
rc=$?
end=$EPOCHREALTIME
# Whatever the run left behind in this namespace (detached daemons).
ps -eo pid,ppid,etimes,time,comm,args > "$out/leftover.txt"
ps -eo pid=,comm= > "$out/.pids"
left=()
while read -r pid comm; do
  [[ $pid =~ ^[0-9]+$ ]] || continue
  [[ $pid -eq 1 || $comm == ps ]] && continue
  left+=("$pid")
done < "$out/.pids"
rm -f "$out/.pids"
# Only the processes of this namespace, all started by this run, are signalled,
# so their CPU lands in the accounting file before it is closed.
if ((${#left[@]})); then
  kill -TERM "${left[@]}" 2>/dev/null
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    alive=0
    for p in "${left[@]}"; do kill -0 "$p" 2>/dev/null && alive=1; done
    ((alive)) || break
    sleep 0.5
  done
  kill -KILL "${left[@]}" 2>/dev/null
  wait 2>/dev/null
fi
printf '{"rc":%d,"start":%s,"end":%s,"leftover":%d}\n' "$rc" "$start" "$end" "${#left[@]}" > "$out/timing.json"
accton off >/dev/null
