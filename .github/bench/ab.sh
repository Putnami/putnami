#!/usr/bin/env bash
# Runs several variants back to back on the same runner. Each variant starts from a cold Go cache.
set +e
out="$1"; shift
lscpu | grep -E 'Model name|^CPU\(s\)' > "$out/cpu.txt"
for step in "$@"; do
  rm -rf "$HOME/.putnami/cache/go/build" "$RUNNER_TEMP/gocache-run" .putnami/stats
  go clean -cache -testcache >/dev/null 2>&1
  mkdir -p "$out/$step"
  ( while true; do ps -eo args | grep -E '(^| )go test' | grep -v grep | head -3 >> "$out/$step/gotest-args.txt"; sleep 10; done ) &
  SP=$!
  case "$step" in
    put-cli-race)    cmd=(./putnamiw test --projects @putnami/cli --no-coverage) ;;
    put-cli-norace)  cmd=(./putnamiw test --projects @putnami/cli --no-coverage --no-race) ;;
    raw-cli-race)    cmd=(bash -c 'cd tooling/cli && GOCACHE="$RUNNER_TEMP/gocache-run" CGO_ENABLED=1 go test -race ./...') ;;
    raw-cli-norace)  cmd=(bash -c 'cd tooling/cli && GOCACHE="$RUNNER_TEMP/gocache-run" go test ./...') ;;
    put-all-cov)     cmd=(./putnamiw test --all) ;;
    put-all-nocov)   cmd=(./putnamiw test --all --no-coverage) ;;
    inst-*)          cmd=(bash -c 'cd tooling/cli && GOCACHE="$RUNNER_TEMP/gocache-run" go test -count=1 -v ./internal/installscript/') ;;
    put-cli-*)       cmd=(./putnamiw test --projects @putnami/cli) ;;
  esac
  # A step named *-old* runs the smoke scripts of BASE_REF; every other step runs the checked-out ones.
  git checkout -q -- tooling/cli/scripts
  case "$step" in
    *-old*) for f in smoke-check-release.sh smoke-first-use.sh; do git show "$BASE_REF:tooling/cli/scripts/$f" > "tooling/cli/scripts/$f"; done ;;
  esac
  # ./putnamiw keys its CLI build on tooling/, so each script variant builds its CLI here, outside the timing.
  ./putnamiw --version >/dev/null 2>&1
  echo "$step procs=$(ls -d /proc/[0-9]* | wc -l) scripts=$(git diff --stat -- tooling/cli/scripts | tail -1)" >> "$out/procs.txt"
  /usr/bin/time -v -o "$out/$step/time.txt" "${cmd[@]}" > "$out/$step/run.log" 2>&1
  kill $SP
  sort -u "$out/$step/gotest-args.txt" -o "$out/$step/gotest-args.txt"
  echo "$step $(grep Elapsed "$out/$step/time.txt" | awk '{print $NF}') $(awk '/User time|System time/{s+=$NF} END{printf "%.1f CPU-min", s/60}' "$out/$step/time.txt")" | tee -a "$out/summary.txt"
done
