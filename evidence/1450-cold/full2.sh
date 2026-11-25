#!/usr/bin/env bash
D=/home/user/bench; unset SELECTION
git -C /home/user/wt-B checkout -q --detach bd5127978e28b3d966096d8a8ba2edd5fe4a9282
(cd /home/user/wt-B && ./putnamiw --version >/dev/null 2>&1)
export LOAD_GATE=1.0
$D/run.sh full-v5-cold-B5 /home/user/wt-B cold
$D/run.sh full-cold-A4 /home/user/wt-A cold
echo FULL2-DONE
