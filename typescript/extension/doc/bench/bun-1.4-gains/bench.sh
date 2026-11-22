#!/usr/bin/env bash
# Measure the Bun 1.4.1 to 1.4.5 gains: see README.md for the method and the results.
#
#   bench.sh sql|idle|startup|isolate|install|all
#
# Runs from the workspace root. Raw results land in $BENCH_WORK as JSON lines;
# `bun measure.ts stats <file> <keys> <metric>` summarizes them.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../../../.." && pwd)"
before="${BENCH_BUN_BEFORE:?set BENCH_BUN_BEFORE to a bun before 1.4.1}"
after="${BENCH_BUN_AFTER:?set BENCH_BUN_AFTER to bun 1.4.5 or later}"
work="${BENCH_WORK:-${TMPDIR:-/tmp}/putnami-bun-1.4-bench}"
runs="${BENCH_RUNS:-10}"
sample="${BENCH_SAMPLE:-typescript/samples/02-rest-api}"
entry="$root/$sample/.gen/src/serve.bundled.ts"
measure=("$after" "$here/measure.ts")
mkdir -p "$work"
cd "$root"

need_entry() {
  [ -f "$entry" ] || { echo "missing $entry: run ./putnamiw build --projects <sample> first" >&2; exit 1; }
}

# Point SELECT through Bun.SQL and postgres.js. Needs DATABASE_URL (default
# postgres://postgres:postgres@127.0.0.1:5432/bench) with a seeded `point` table.
sql() {
  export BENCH_POSTGRES_JS="${BENCH_POSTGRES_JS:-$(readlink -f typescript/framework/database/node_modules/postgres)/src/index.js}"
  psql "${DATABASE_URL:-postgres://postgres:postgres@127.0.0.1:5432/bench}" -q -c \
    "create table if not exists point(id int primary key, name text, score double precision);
     insert into point select g, 'name-'||g, g*1.5 from generate_series(1,1000) g on conflict do nothing;"
  for _ in $(seq "$runs"); do
    for bun in "$before" "$after"; do
      for driver in bun postgresjs; do "$bun" "$here/measure.ts" sql "$driver" 20000; done
    done
  done >"$work/sql.jsonl"
  "${measure[@]}" stats "$work/sql.jsonl" bun,driver cpuUsPerQuery
}

# Idle CPU of the sample over 120 s, both versions side by side.
idle() {
  need_entry
  for _ in $(seq "${BENCH_IDLE_RUNS:-5}"); do
    (cd "$sample" && "${measure[@]}" idle 120 "$before" "$entry") &
    local first=$!
    (cd "$sample" && BENCH_BASE_PORT=45280 "${measure[@]}" idle 120 "$after" "$entry") &
    local second=$!
    wait "$first"
    wait "$second"
  done >"$work/idle.jsonl"
  "${measure[@]}" stats "$work/idle.jsonl" cmd idleCpuMs
}

# --bytecode-order: compile as typescript/extension/internal/build/compile.go
# does, record an order file from one run, recompile with it, compare start-up.
startup() {
  need_entry
  local out="$work/compile"
  mkdir -p "$out"
  rm -f "$out/plain" "$out/bytecode" "$out/bytecode-ordered" "$out/order.txt"
  local args=(build --compile --format=esm "$entry" --target bun-linux-x64)
  "$after" "${args[@]}" --outfile "$out/plain" >/dev/null
  "$after" "${args[@]}" --bytecode --outfile "$out/bytecode" >/dev/null
  PORT=45399 BUN_BYTECODE_ORDER_OUT="$out/order.txt" timeout 5 "$out/bytecode" >/dev/null 2>&1 || true
  [ -s "$out/order.txt" ] || { echo "the training run wrote no order file" >&2; exit 1; }
  "$after" "${args[@]}" --bytecode --bytecode-order="$out/order.txt" --outfile "$out/bytecode-ordered" >/dev/null
  {
    "${measure[@]}" startup "${BENCH_WARM_STARTS:-30}" warm "$out/plain" "$out/bytecode" "$out/bytecode-ordered"
    # Dropping the page cache needs root; skip the cold rows without it.
    if sh -c 'echo 3 > /proc/sys/vm/drop_caches' 2>/dev/null; then
      "${measure[@]}" startup "${BENCH_COLD_STARTS:-30}" cold "$out/plain" "$out/bytecode" "$out/bytecode-ordered"
    fi
  } >"$work/startup.jsonl"
  "${measure[@]}" stats "$work/startup.jsonl" bin,cache ms
}

# bun test with and without --isolate on the framework packages with the most files.
isolate() {
  for _ in $(seq "${BENCH_TEST_RUNS:-5}"); do
    for project in ${BENCH_TEST_PROJECTS:-application web}; do
      for flag in none --isolate; do
        local extra=()
        [ "$flag" = none ] || extra=("$flag")
        printf '{"project":"%s","flag":"%s",' "$project" "$flag"
        "${measure[@]}" rss "typescript/framework/$project" "$after" test "${extra[@]}" | sed 's/^{//'
      done
    done
  done >"$work/isolate.jsonl"
  "${measure[@]}" stats "$work/isolate.jsonl" project,flag maxRssMB
  "${measure[@]}" stats "$work/isolate.jsonl" project,flag wallS
}

# bun install with the root bunfig.toml (isolated linker) in a clean worktree:
# cold (empty cache), warm (cache kept, node_modules removed), no-op.
install() {
  local tree="$work/worktree"
  git worktree remove --force "$tree" 2>/dev/null || rm -rf "$tree"
  git worktree add --quiet --detach "$tree" HEAD
  # shellcheck disable=SC2064 # expand now: tree is local to this function
  trap "git -C '$root' worktree remove --force '$tree'" EXIT
  for i in $(seq "${BENCH_INSTALL_RUNS:-5}"); do
    for bun in "$before" "$after"; do
      local cache="$work/install-cache"
      rm -rf "$cache"
      (cd "$tree" && find . -name node_modules -type d -prune -exec rm -rf {} +)
      local t0 t1 t2 t3 t4
      t0=$(date +%s%N)
      (cd "$tree" && BUN_INSTALL_CACHE_DIR="$cache" "$bun" install --frozen-lockfile >/dev/null)
      t1=$(date +%s%N)
      (cd "$tree" && find . -name node_modules -type d -prune -exec rm -rf {} +)
      t2=$(date +%s%N)
      (cd "$tree" && BUN_INSTALL_CACHE_DIR="$cache" "$bun" install --frozen-lockfile >/dev/null)
      t3=$(date +%s%N)
      (cd "$tree" && BUN_INSTALL_CACHE_DIR="$cache" "$bun" install --frozen-lockfile >/dev/null)
      t4=$(date +%s%N)
      printf '{"bun":"%s","run":%d,"coldMs":%d,"warmMs":%d,"noopMs":%d}\n' "$("$bun" --version)" "$i" \
        $(((t1 - t0) / 1000000)) $(((t3 - t2) / 1000000)) $(((t4 - t3) / 1000000))
    done
  done >"$work/install.jsonl"
  for metric in coldMs warmMs noopMs; do "${measure[@]}" stats "$work/install.jsonl" bun "$metric"; done
}

case "${1:-all}" in
  sql | idle | startup | isolate | install) "$1" ;;
  all)
    sql
    idle
    startup
    isolate
    install
    ;;
  *) echo "usage: bench.sh sql|idle|startup|isolate|install|all" >&2; exit 2 ;;
esac
