#!/usr/bin/env bash
# The whole campaign, runs one after another, never overlapping.
set -u
D=$(cd "$(dirname "$0")" && pwd)
A=/home/user/wt-A; B=/home/user/wt-B
# 0. Warm-up: one full run per worktree (installs, toolchains); not in the series.
"$D/run.sh" warmup-A "$A" cold
"$D/run.sh" warmup-B "$B" cold
# 1. No-cache gate, empty task history.
"$D/run.sh" cold-A1 "$A" cold
"$D/run.sh" cold-B1 "$B" cold
"$D/run.sh" cold-A2 "$A" cold
"$D/run.sh" cold-B2 "$B" cold
# 2. No-cache gate, warm history: each commit's history after its first cold run.
mkdir -p "$D/history"
cp "$D/runs/cold-A1/tasks-after.json" "$D/history/A-tasks.json"
cp "$D/runs/cold-B1/tasks-after.json" "$D/history/B-tasks.json"
"$D/run.sh" warm-A1 "$A" warm "$D/history/A-tasks.json"
"$D/run.sh" warm-B1 "$B" warm "$D/history/B-tasks.json"
"$D/run.sh" warm-A2 "$A" warm "$D/history/A-tasks.json"
"$D/run.sh" warm-B2 "$B" warm "$D/history/B-tasks.json"
# 3. Unchanged workspace (cache on), after the full runs above.
"$D/run.sh" unchanged-A1 "$A" unchanged
"$D/run.sh" unchanged-B1 "$B" unchanged
"$D/run.sh" unchanged-A2 "$A" unchanged
"$D/run.sh" unchanged-B2 "$B" unchanged
echo CAMPAIGN-DONE
