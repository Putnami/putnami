#!/usr/bin/env bash
# Selection protocol: cold x3 and warm x2, A (main) and B alternating.
# Usage: campaign.sh <tag> <B-commit>
set -u
D=/home/user/bench; . $D/sel.env
TAG=$1; BC=$2
A=/home/user/wt-A; B=/home/user/wt-B
git -C $B checkout -q --detach "$BC"
(cd $B && ./putnamiw --version >/dev/null 2>&1)
for i in 1 2 3; do
  $D/run.sh $TAG-sel-cold-A$i $A cold
  $D/run.sh $TAG-sel-cold-B$i $B cold
done
$D/run.sh $TAG-sel-seed-A $A cold
$D/run.sh $TAG-sel-seed-B $B cold
mkdir -p $D/history
cp $D/runs/$TAG-sel-seed-A/tasks-after.json $D/history/$TAG-sel-A-tasks.json
cp $D/runs/$TAG-sel-seed-B/tasks-after.json $D/history/$TAG-sel-B-tasks.json
for i in 1 2; do
  $D/run.sh $TAG-sel-warm-A$i $A warm $D/history/$TAG-sel-A-tasks.json
  $D/run.sh $TAG-sel-warm-B$i $B warm $D/history/$TAG-sel-B-tasks.json
done
echo CAMPAIGN-$TAG-DONE
