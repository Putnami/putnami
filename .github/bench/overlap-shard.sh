#!/usr/bin/env bash
# Per-test coverage of the CLI suite, one shard of it.
# Usage: overlap-shard.sh <shard> <shards> <out>
# Each top-level test runs alone in its own process. GOCOVERDIR is set for the
# test process and inherited by the children it re-executes, so the CLI
# sessions an end-to-end test starts count toward that test.
set -u
shard="$1"; shards="$2"; out="$(mkdir -p "$3" && cd "$3" && pwd)"
mod=go.putnami.dev/tooling/cli
cpus="$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN)"
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
to=timeout; command -v timeout >/dev/null || to=gtimeout; command -v $to >/dev/null || to=""
export cpus to; export -f now
work="${RUNNER_TEMP:-/tmp}/overlap"
mkdir -p "$work/bin" "$out/cov" "$out/universe" "$out/baseline"
cd tooling/cli

# Every shard builds every test binary: items are dealt round-robin.
go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}} {{.Dir}}{{end}}' ${PKGS:-./...} > "$work/pkgs.txt"
build() {
  ip="$1"; name="$(echo "$ip" | tr '/.' '__')"
  go test -c -cover -covermode=set -coverpkg="$mod/..." -o "$work/bin/$name.test" "$ip" > "$work/bin/$name.build.log" 2>&1 \
    || echo "build failed: $ip" >&2
}
export -f build; export work mod
start=$(date +%s)
cut -d' ' -f1 "$work/pkgs.txt" | xargs -P"$cpus" -I{} bash -c 'build {}'
echo "build_seconds $(( $(date +%s) - start ))" > "$out/shard-$shard.meta"

# One item per top-level test, sorted, then dealt to the shards.
: > "$work/items.txt"
while read -r ip dir; do
  name="$(echo "$ip" | tr '/.' '__')"
  [ -x "$work/bin/$name.test" ] || continue
  (cd "$dir" && "$work/bin/$name.test" -test.list '^Test' 2>/dev/null) \
    | grep -E '^Test' | while read -r t; do echo "$name $dir $t"; done >> "$work/items.txt"
done < "$work/pkgs.txt"
sort -o "$work/items.txt" "$work/items.txt"
awk -v s="$shard" -v n="$shards" '(NR-1)%n==s' "$work/items.txt" > "$work/mine.txt"
echo "items_total $(wc -l < "$work/items.txt")" >> "$out/shard-$shard.meta"
echo "items_shard $(wc -l < "$work/mine.txt")" >> "$out/shard-$shard.meta"

# Tests of one package run one after the other, as go test runs them;
# packages run in parallel, as go test runs them.
runpkg() {
  name="$1"
  # What the binary reaches with no test: init and TestMain. Every test of the
  # package reaches it, so the report subtracts it.
  dir0="$(grep -m1 "^$name " "$work/mine.txt" | cut -d' ' -f2)"
  b="$work/cd/$name.baseline"; rm -rf "$b"; mkdir -p "$b"
  (cd "$dir0" && GOCOVERDIR="$b" ${to:+$to 900} "$work/bin/$name.test" -test.run '^$' -test.gocoverdir="$b" > /dev/null 2>&1)
  go tool covdata textfmt -i="$b" -o "$b.txt" 2>/dev/null
  awk 'NR>1 && $3>0 {print $1, $2}' "$b.txt" 2>/dev/null | gzip -c > "$out/baseline/$name.gz"
  rm -rf "$b" "$b.txt"
  grep "^$name " "$work/mine.txt" | while read -r _ dir t; do
    d="$work/cd/$name.$t"; rm -rf "$d"; mkdir -p "$d"
    s=$(now)
    (cd "$dir" && GOCOVERDIR="$d" ${to:+$to 900} "$work/bin/$name.test" \
      -test.run "^$t\$" -test.count=1 -test.v -test.timeout=14m -test.gocoverdir="$d" \
      > "$work/log.$name.$t" 2>&1)
    code=$?
    e=$(now)
    el=$(grep -m1 -E "^--- (PASS|FAIL|SKIP): $t \(" "$work/log.$name.$t" | sed -E 's/.*\(([0-9.]+)s\).*/\1/')
    st=$(grep -m1 -oE "^--- (PASS|FAIL|SKIP): $t " "$work/log.$name.$t" | awk '{print $2}' | tr -d :)
    f="$out/cov/$name.$t"
    go tool covdata textfmt -i="$d" -o "$d.txt" 2>/dev/null
    # Keep covered blocks only: "file:start,end numStmt".
    awk 'NR>1 && $3>0 {print $1, $2}' "$d.txt" 2>/dev/null | gzip -c > "$f.gz"
    # The block universe of this binary, once per package.
    [ -f "$out/universe/$name.gz" ] || awk 'NR>1 {print $1, $2}' "$d.txt" 2>/dev/null | gzip -c > "$out/universe/$name.gz"
    printf '%s\t%s\t%s\t%s\t%s\t%.3f\n' "$name" "$dir" "$t" "${st:-NONE}" "${el:-}" "$(awk -v a="$s" -v b="$e" 'BEGIN{print b-a}')" >> "$out/runs-$shard.$name.tsv"
    if [ "$code" != 0 ]; then tail -n 30 "$work/log.$name.$t" > "$out/cov/$name.$t.fail.log"; fi
    rm -rf "$d" "$d.txt" "$work/log.$name.$t"
  done
}
export -f runpkg; export out shard
start=$(date +%s)
cut -d' ' -f1 "$work/mine.txt" | sort -u | xargs -P"$cpus" -I{} bash -c 'runpkg {}'
echo "run_seconds $(( $(date +%s) - start ))" >> "$out/shard-$shard.meta"
cat "$out"/runs-"$shard".*.tsv > "$out/runs-$shard.tsv" 2>/dev/null; rm -f "$out"/runs-"$shard".*.tsv

echo "shard $shard done"
