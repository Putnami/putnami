#!/usr/bin/env bash
D=/home/user/bench; . $D/sel.env
$D/run.sh warmup-sel-A /home/user/wt-A cold
$D/run.sh warmup-sel-B /home/user/wt-B cold
echo WARMUP-DONE
