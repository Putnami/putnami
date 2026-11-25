#!/usr/bin/env bash
# One measured run of the Putnami gate in a worktree.
# Usage: run.sh <label> <worktree> <mode: cold|warm|unchanged> [history-file]
#   cold      : .putnami/stats/tasks.json removed, --no-cache
#   warm      : tasks.json restored from <history-file>, --no-cache
#   unchanged : tasks.json left as is, cache on
# SELECTION=<a>,<b> in the environment replaces --all with --projects <a>,<b>.
# Waits for the one-minute load to fall below $LOAD_GATE (default 1.0), samples /proc/loadavg every
# 15 s from just before the start to the end, and copies the run's report and
# session.json into runs/<label>/.
set -u
LABEL=$1; WT=$2; MODE=$3; HIST=${4:-}
EVID=$(cd "$(dirname "$0")" && pwd)
OUT="$EVID/runs/$LABEL"; mkdir -p "$OUT"
LOGDIR=${LOGDIR:-/tmp/1450-logs}; mkdir -p "$LOGDIR"

LOAD_GATE=${LOAD_GATE:-1.0}
until awk -v g="$LOAD_GATE" '{exit !($1 < g)}' /proc/loadavg; do sleep 5; done

cd "$WT"
case "$MODE" in
  cold)      rm -f .putnami/stats/tasks.json; ARGS="--no-cache" ;;
  warm)      mkdir -p .putnami/stats; cp "$HIST" .putnami/stats/tasks.json; ARGS="--no-cache" ;;
  unchanged) ARGS="" ;;
  *) echo "bad mode"; exit 2 ;;
esac
ls .putnami/sessions 2>/dev/null | grep -E "^[0-9]{8}-[0-9]{6}-" | sort > "$LOGDIR/$LABEL.before"

( while :; do echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) $(cat /proc/loadavg)"; sleep 15; done ) > "$OUT/load.txt" &
SAMPLER=$!
START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
SCOPE="--all"; [ -n "${SELECTION:-}" ] && SCOPE="--projects $SELECTION"
./putnamiw lint,test,build,validate $SCOPE $ARGS --enforce-coverage > "$LOGDIR/$LABEL.log" 2>&1
RC=$?
END=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kill $SAMPLER; wait $SAMPLER 2>/dev/null
echo "$END $(cat /proc/loadavg)" >> "$OUT/load.txt"

SID=$(ls .putnami/sessions | grep -E "^[0-9]{8}-[0-9]{6}-" | sort | comm -13 "$LOGDIR/$LABEL.before" - | tail -1)
cp ".putnami/reports/$SID.json" "$OUT/report.json" 2>/dev/null
cp ".putnami/sessions/$SID/session.json" "$OUT/session.json" 2>/dev/null
[ -f .putnami/stats/tasks.json ] && cp .putnami/stats/tasks.json "$OUT/tasks-after.json"
tail -40 "$LOGDIR/$LABEL.log" > "$OUT/log-tail.txt"
printf '{"label":"%s","commit":"%s","mode":"%s","session":"%s","start":"%s","end":"%s","exitCode":%d,"args":"%s %s"}\n' \
  "$LABEL" "$(git rev-parse HEAD)" "$MODE" "$SID" "$START" "$END" "$RC" "$SCOPE" "$ARGS" > "$OUT/run.json"
cat "$OUT/run.json"
