#!/usr/bin/env bash
while pgrep -f warmup.sh >/dev/null; do sleep 10; done
/home/user/bench/campaign.sh v1 ef43639f
