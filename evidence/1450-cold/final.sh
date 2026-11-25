#!/usr/bin/env bash
D=/home/user/bench
$D/campaign.sh v6 d9ab71576ffd141b3b3f7cbb5c9249e0b847ed55
unset SELECTION
export LOAD_GATE=1.0
$D/run.sh full-v6-cold-B6 /home/user/wt-B cold
$D/run.sh full-cold-A5 /home/user/wt-A cold
echo FINAL-DONE
