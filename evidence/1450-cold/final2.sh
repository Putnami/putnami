#!/usr/bin/env bash
D=/home/user/bench; unset SELECTION
export LOAD_GATE=1.0
$D/run.sh full-v6-cold-B7 /home/user/wt-B cold
$D/run.sh full-cold-A6 /home/user/wt-A cold
echo FINAL2-DONE
