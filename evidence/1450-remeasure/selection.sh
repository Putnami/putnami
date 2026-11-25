#!/usr/bin/env bash
# Second protocol: a project selection instead of the full gate.
set -u
D=$(cd "$(dirname "$0")" && pwd)
A=/home/user/wt-A; B=/home/user/wt-B
export LOAD_GATE=2.0
export SELECTION=/go/framework/client,/go/framework/api,/go/framework/database,/go/framework/app,/go/framework/openapi,/go/framework/grpc,/go/framework/telemetry,/typescript/framework/client,/typescript/framework/application,/typescript/framework/web,/typescript/framework/analytics,/typescript/framework/cli-protocol,/typescript/framework/ui,/tooling/cli-documents
# Let the full-gate run in progress end first; runs never overlap.
while pgrep -f "run.sh cold-B1" >/dev/null; do sleep 10; done
# 1. No-cache, empty task history: A, B alternating, 3 each.
for i in 1 2 3; do
  "$D/run.sh" sel-cold-A$i "$A" cold
  "$D/run.sh" sel-cold-B$i "$B" cold
done
# 2. Warm history: one seeding run per commit (empty history), then A, B alternating, 2 each.
"$D/run.sh" sel-seed-A "$A" cold
"$D/run.sh" sel-seed-B "$B" cold
mkdir -p "$D/history"
cp "$D/runs/sel-seed-A/tasks-after.json" "$D/history/sel-A-tasks.json"
cp "$D/runs/sel-seed-B/tasks-after.json" "$D/history/sel-B-tasks.json"
for i in 1 2; do
  "$D/run.sh" sel-warm-A$i "$A" warm "$D/history/sel-A-tasks.json"
  "$D/run.sh" sel-warm-B$i "$B" warm "$D/history/sel-B-tasks.json"
done
echo SELECTION-DONE
