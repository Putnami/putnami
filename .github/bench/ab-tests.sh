#!/usr/bin/env bash
# Same-runner A/B of the CLI test. A step named *-old* runs the CLI tests of BASE_REF, any other step those of HEAD.
# Each step starts from a cold Go cache with the Putnami cache off.
set +e
out="$1"; shift
lscpu | grep -E 'Model name|^CPU\(s\)' > "$out/cpu.txt"
for step in "$@"; do
  case "$step" in *-old*) src="$BASE_REF" ;; *) src=HEAD ;; esac
  rm -rf tooling/cli/internal/cli && git checkout -q "$src" -- tooling/cli/internal/cli
  rm -rf "$HOME/.putnami/cache/go/build" .putnami/stats .putnami/sessions
  go clean -cache -testcache >/dev/null 2>&1
  mkdir -p "$out/$step"
  ./putnamiw --version >/dev/null 2>&1
  /usr/bin/time -v -o "$out/$step/time.txt" ./putnamiw test --projects @putnami/cli --no-race-changed --no-cache > "$out/$step/run.log" 2>&1
  echo "exit=$?" >> "$out/$step/time.txt"
  cp -r .putnami/sessions "$out/$step/" 2>/dev/null
  echo "$step $(grep Elapsed "$out/$step/time.txt" | awk '{print $NF}') $(awk '/User time|System time/{s+=$NF} END{printf "%.1f CPU-min", s/60}' "$out/$step/time.txt") $(grep exit= "$out/$step/time.txt")" | tee -a "$out/summary.txt"
done
