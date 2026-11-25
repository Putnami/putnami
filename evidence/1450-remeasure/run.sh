#!/usr/bin/env bash
# One measured run of the Putnami gate in a worktree.
# Usage: run.sh <label> <worktree> <mode: cold|warm|unchanged> [history-file]
#   cold      : .putnami/stats/tasks.json removed, --no-cache
#   warm      : tasks.json restored from <history-file>, --no-cache
#   unchanged : tasks.json left as is, cache on
# Waits for the one-minute load to fall below 1.0, samples /proc/loadavg every
# 15 s from just before the start to the end, and copies the run's report and
# session.json into runs/<label>/.
set -u
LABEL=$1; WT=$2; MODE=$3; HIST=${4:-}
EVID=$(cd "$(dirname "$0")" && pwd)
OUT="$EVID/runs/$LABEL"; mkdir -p "$OUT"
LOGDIR=${LOGDIR:-/tmp/1450-logs}; mkdir -p "$LOGDIR"

until awk '{exit !($1 < 1.0)}' /proc/loadavg; do sleep 5; done

cd "$WT"
case "$MODE" in
  cold)      rm -f .putnami/stats/tasks.json; ARGS="--no-cache" ;;
  warm)      mkdir -p .putnami/stats; cp "$HIST" .putnami/stats/tasks.json; ARGS="--no-cache" ;;
  unchanged) ARGS="" ;;
  *) echo "bad mode"; exit 2 ;;
esac
ls .putnami/sessions 2>/dev/null | sort > "$LOGDIR/$LABEL.before"

( while :; do echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) $(cat /proc/loadavg)"; sleep 15; done ) > "$OUT/load.txt" &
SAMPLER=$!
START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
./putnamiw lint,test,build,validate --all $ARGS --enforce-coverage > "$LOGDIR/$LABEL.log" 2>&1
RC=$?
END=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kill $SAMPLER; wait $SAMPLER 2>/dev/null
echo "$END $(cat /proc/loadavg)" >> "$OUT/load.txt"

SID=$(ls .putnami/sessions | sort | comm -13 "$LOGDIR/$LABEL.before" - | tail -1)
cp ".putnami/reports/$SID.json" "$OUT/report.json" 2>/dev/null
cp ".putnami/sessions/$SID/session.json" "$OUT/session.json" 2>/dev/null
[ -f .putnami/stats/tasks.json ] && cp .putnami/stats/tasks.json "$OUT/tasks-after.json"
tail -40 "$LOGDIR/$LABEL.log" > "$OUT/log-tail.txt"
printf '{"label":"%s","commit":"%s","mode":"%s","session":"%s","start":"%s","end":"%s","exitCode":%d,"args":"%s"}\n' \
  "$LABEL" "$(git rev-parse HEAD)" "$MODE" "$SID" "$START" "$END" "$RC" "$ARGS" > "$OUT/run.json"
cat "$OUT/run.json"
