#!/usr/bin/env bash
# Full no-cache gate pair: one warm-up per worktree (installs, toolchains; not compared),
# then cold-A, cold-B from an empty task history, load gate 1.0.
set -u
D=/home/user/bench
unset SELECTION
export LOAD_GATE=2.0
$D/run.sh full-warmup-A /home/user/wt-A unchanged
$D/run.sh full-warmup-B /home/user/wt-B unchanged
export LOAD_GATE=1.0
$D/run.sh full-cold-A1 /home/user/wt-A cold
$D/run.sh full-cold-B1 /home/user/wt-B cold
echo FULL-DONE
