#!/usr/bin/env bash
# Benchmark the economics of the shared build CAS across repeatable cache states.
#
# The harness never mutates the caller's checkout: every trial runs in a temporary
# git worktree pinned to CACHE_BENCH_BASELINE (origin/main by default). It writes
# one labeled JSON object per trial plus p50/p95 reports to CACHE_BENCH_OUTPUT_DIR.
#
# Required cache access:
#   CACHE_BENCH_S0_NAMESPACE_COMMAND  provisions an empty S0 namespace per trial
#   CACHE_BENCH_S1_READONLY_CACHE_URL trusted CI cache endpoint for S1 and S2
#   CACHE_BENCH_S1_READONLY_CACHE_TOKEN read-only credential for that endpoint
#
# See tooling/cli/doc/12-profiling-and-telemetry.md for the state definitions,
# data contract, and safe setup instructions.

set -euo pipefail

script_name="$(basename "$0")"
readonly script_name

die() {
  printf '%s\n' "${script_name}: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

usage() {
  printf '%s\n' \
    "Usage: ${script_name}" \
    "" \
    "Runs the S0/S1/S2 × change-class matrix with five trials per cell by default." \
    "Results: .putnami/cache-economics/<UTC timestamp>/{runs.jsonl,report.json,report.md}" \
    "" \
    "Required environment:" \
    "  CACHE_BENCH_S0_NAMESPACE_COMMAND  executable that provisions an empty S0 namespace per trial" \
    "  CACHE_BENCH_S1_READONLY_CACHE_URL trusted CI-seeded cache endpoint for S1/S2" \
    "  CACHE_BENCH_S1_READONLY_CACHE_TOKEN read-only credential for the S1/S2 endpoint" \
    "" \
    "Optional environment:" \
    "  CACHE_BENCH_BASELINE       git ref to seed and compare (origin/main)" \
    "  CACHE_BENCH_TRIALS         trials per cell (5; fewer require CACHE_BENCH_ALLOW_FEWER_TRIALS=1)" \
    "  CACHE_BENCH_OUTPUT_DIR     result directory" \
    "  CACHE_BENCH_MACHINE        durable machine label (auto-detected by default)" \
    "  CACHE_BENCH_COMMAND        command words without selection flags (lint,test,build)" \
    "  CACHE_BENCH_STATES         comma-separated subset of S0,S1,S2" \
    "  CACHE_BENCH_CHANGE_CLASSES comma-separated subset of no-change,docs-only,ts-leaf,go-leaf,protocol-wide,e2e-sample" \
    "  CACHE_BENCH_REQUIRE_REMOTE set 0 only for local-cache diagnostics; published datasets require 1"
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi
if [[ $# -gt 0 ]]; then
  usage >&2
  exit 2
fi

repo_root="$(git -C "$(dirname "$0")/../../.." rev-parse --show-toplevel 2>/dev/null)" || die "must run from a git checkout"
cd "$repo_root"

require_command git
require_command jq
[[ -x /usr/bin/time ]] || die "requires BSD /usr/bin/time with -l support"

putnami_bin="${PUTNAMI_BIN:-putnami}"
case "$putnami_bin" in
  */*) [[ -x "$putnami_bin" ]] || die "PUTNAMI_BIN is not executable: $putnami_bin" ;;
  *) require_command "$putnami_bin" ;;
esac

baseline_ref="${CACHE_BENCH_BASELINE:-origin/main}"
baseline_sha="$(git rev-parse --verify "${baseline_ref}^{commit}" 2>/dev/null)" || die "baseline is not a commit: $baseline_ref"

trials="${CACHE_BENCH_TRIALS:-5}"
[[ "$trials" =~ ^[1-9][0-9]*$ ]] || die "CACHE_BENCH_TRIALS must be a positive integer"
if (( trials < 5 )) && [[ "${CACHE_BENCH_ALLOW_FEWER_TRIALS:-0}" != "1" ]]; then
  die "CACHE_BENCH_TRIALS must be at least 5 (set CACHE_BENCH_ALLOW_FEWER_TRIALS=1 for a diagnostic run)"
fi

IFS=',' read -r -a states <<< "${CACHE_BENCH_STATES:-S0,S1,S2}"
IFS=',' read -r -a change_classes <<< "${CACHE_BENCH_CHANGE_CLASSES:-no-change,docs-only,ts-leaf,go-leaf,protocol-wide,e2e-sample}"
IFS=' ' read -r -a command_args <<< "${CACHE_BENCH_COMMAND:-lint,test,build}"
(( ${#command_args[@]} > 0 )) || die "CACHE_BENCH_COMMAND must contain command words"

for state in "${states[@]}"; do
  case "$state" in
    S0 | S1 | S2) ;;
    *) die "unknown cache state: $state" ;;
  esac
done
for change_class in "${change_classes[@]}"; do
  case "$change_class" in
    no-change | docs-only | ts-leaf | go-leaf | protocol-wide | e2e-sample) ;;
    *) die "unknown change class: $change_class" ;;
  esac
done

require_remote="${CACHE_BENCH_REQUIRE_REMOTE:-1}"
[[ "$require_remote" == "0" || "$require_remote" == "1" ]] || die "CACHE_BENCH_REQUIRE_REMOTE must be 0 or 1"

utc_date="$(date -u +%F)"
run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
machine_default="$(sysctl -n hw.model 2>/dev/null || uname -s)-$(uname -m)"
machine="${CACHE_BENCH_MACHINE:-$machine_default}"
output_dir="${CACHE_BENCH_OUTPUT_DIR:-$repo_root/.putnami/cache-economics/$run_id}"
runs_file="$output_dir/runs.jsonl"
report_json="$output_dir/report.json"
report_md="$output_dir/report.md"
mkdir -p "$output_dir"
: > "$runs_file"

state_selected() {
  local expected="$1"
  local state
  for state in "${states[@]}"; do
    [[ "$state" == "$expected" ]] && return 0
  done
  return 1
}

s0_namespace_command="${CACHE_BENCH_S0_NAMESPACE_COMMAND:-}"
s1_readonly_cache_url="${CACHE_BENCH_S1_READONLY_CACHE_URL:-}"
s1_readonly_cache_token="${CACHE_BENCH_S1_READONLY_CACHE_TOKEN:-}"

# S0 must use a fresh cache identity for every class/trial. A URL is not a
# sufficient namespace boundary: cache providers normally derive that boundary
# from the credential. The provisioner receives the requested namespace ID and
# returns the matching URL and one-time credential without either being written
# to the benchmark dataset.
if state_selected S0; then
  [[ -n "$s0_namespace_command" ]] || die "CACHE_BENCH_S0_NAMESPACE_COMMAND is required for S0"
  [[ -x "$s0_namespace_command" ]] || die "CACHE_BENCH_S0_NAMESPACE_COMMAND is not executable: $s0_namespace_command"
fi

# Every access to the CI-seeded cache, including the local seed, is explicitly
# configured with a read-only credential. This prevents a benchmark miss from
# uploading platform-specific or absent entries into the trusted CI namespace.
if state_selected S1 || state_selected S2; then
  [[ -n "$s1_readonly_cache_url" ]] || die "CACHE_BENCH_S1_READONLY_CACHE_URL is required for S1/S2"
  [[ -n "$s1_readonly_cache_token" ]] || die "CACHE_BENCH_S1_READONLY_CACHE_TOKEN is required for S1/S2"
fi

work_root="$(mktemp -d "${TMPDIR:-/tmp}/putnami-cache-bench.XXXXXX")"
declare -a worktrees=()

cleanup() {
  local status=$?
  trap - EXIT
  local worktree
  for worktree in "${worktrees[@]:-}"; do
    git -C "$repo_root" worktree remove --force "$worktree" >/dev/null 2>&1 || true
  done
  if [[ "${CACHE_BENCH_KEEP_WORKTREES:-0}" != "1" ]]; then
    rm -rf "$work_root"
  else
    printf '%s\n' "${script_name}: retained diagnostic worktrees in $work_root" >&2
  fi
  exit "$status"
}
trap cleanup EXIT

cache_url=""
cache_token=""

configure_s0_namespace() {
  local change_class="$1"
  local trial="$2"
  local namespace_id="${run_id}-S0-${change_class}-${trial}"
  local config
  local returned_namespace_id

  config="$("$s0_namespace_command" "$namespace_id")" || die "S0 namespace provisioner failed for $namespace_id"
  returned_namespace_id="$(jq -er '.namespaceID | strings | select(length > 0)' <<<"$config")" || die "S0 namespace provisioner returned no namespaceID for $namespace_id"
  [[ "$returned_namespace_id" == "$namespace_id" ]] || die "S0 namespace provisioner returned $returned_namespace_id; expected $namespace_id"
  cache_url="$(jq -er '.url | strings | select(length > 0)' <<<"$config")" || die "S0 namespace provisioner returned no URL for $namespace_id"
  cache_token="$(jq -er '.token | strings | select(length > 0)' <<<"$config")" || die "S0 namespace provisioner returned no credential for $namespace_id"
}

configure_cache_access() {
  local state="$1"
  local change_class="$2"
  local trial="$3"

  case "$state" in
    S0)
      configure_s0_namespace "$change_class" "$trial"
      ;;
    S1 | S2)
      cache_url="$s1_readonly_cache_url"
      cache_token="$s1_readonly_cache_token"
      ;;
  esac
}

add_worktree() {
  local path="$1"
  git -C "$repo_root" worktree add --detach "$path" "$baseline_sha" >/dev/null
  worktrees+=("$path")
}

# S1/S2 trials share one merge-base seed store per selection profile (the e2e
# class seeds a different selection than every other class). Sharing preserves
# per-trial semantics: each trial's changed jobs key on a trial-unique marker,
# so the only entries a later trial can hit are merge-base-keyed — exactly what
# its own seed would have produced. Every seed after the first therefore
# becomes a fast local hydrate instead of a full remote restore.
seed_profile_for_class() {
  if [[ "$1" == "e2e-sample" ]]; then
    printf 'e2e'
  else
    printf 'default'
  fi
}

seed_store_for_profile() {
  printf '%s/seed-store-%s' "$work_root" "$1"
}

# Fresh worktrees do not share node_modules with the primary checkout, and the
# first-use auto-install marker is machine-global, so TypeScript type jobs
# would fail on missing dev type definitions. Install runs before every
# measured window on purpose: the fresh-worktree install tax belongs to the
# zero-init bootstrap work, not to this cache-economics matrix.
bootstrap_worktree() {
  local worktree="$1"
  local store_dir="$2"
  local log_dir="$3"
  (
    cd "$worktree"
    env PUTNAMI_STORE_DIR="$store_dir" "$putnami_bin" deps install
  ) >"$log_dir/deps-install.stdout" 2>"$log_dir/deps-install.stderr" || die "worktree deps install failed; inspect $log_dir/deps-install.stderr"
}

remove_worktree() {
  local path="$1"
  git -C "$repo_root" worktree remove --force "$path" >/dev/null
  local retained=()
  local worktree
  for worktree in "${worktrees[@]}"; do
    [[ "$worktree" == "$path" ]] || retained+=("$worktree")
  done
  worktrees=("${retained[@]:-}")
}

selection_args_for_class() {
  local class="$1"
  selection_args=(--impacted --baseline "$baseline_sha")
  if [[ "$class" == "e2e-sample" ]]; then
    # E2E projects are excluded in normal workspace configuration; benchmarking
    # this class intentionally opts into the sample-app slice only.
    selection_args+=(--tag e2e)
  fi
}

apply_change() {
  local worktree="$1"
  local class="$2"
  local marker="$3"
  local path=""
  local line=""

  case "$class" in
    no-change)
      return 0
      ;;
    docs-only)
      path="tooling/cli/doc/12-profiling-and-telemetry.md"
      line="<!-- cache-economics ${marker} -->"
      ;;
    ts-leaf)
      path="typescript/framework/utils/src/index.ts"
      line="// cache-economics ${marker}"
      ;;
    go-leaf)
      path="go/framework/logger/logger.go"
      line="// cache-economics ${marker}"
      ;;
    protocol-wide)
      path="protocols/cache/provider.go"
      line="// cache-economics ${marker}"
      ;;
    e2e-sample)
      path="go/samples/application/main.go"
      line="// cache-economics ${marker}"
      ;;
  esac

  [[ -f "$worktree/$path" ]] || die "benchmark fixture path is missing at ${baseline_sha:0:12}: $path"
  printf '\n%s\n' "$line" >> "$worktree/$path"
}

seed_merge_base() {
  local worktree="$1"
  local profile="$2"
  local change_class="$3"
  local log_dir="$4"
  local store_dir
  store_dir="$(seed_store_for_profile "$profile")"
  local sentinel="$work_root/seed-verified-${profile}"
  local -a seed_args=("${command_args[@]}" --all --output=json)

  if [[ "$change_class" == "e2e-sample" ]]; then
    seed_args+=(--tag e2e)
  fi

  mkdir -p "$store_dir"
  (
    cd "$worktree"
    env PUTNAMI_STORE_DIR="$store_dir" PUTNAMI_CACHE_URL="$cache_url" PUTNAMI_CACHE_TOKEN="$cache_token" \
      "$putnami_bin" "${seed_args[@]}"
  ) >"$log_dir/seed.stdout" 2>"$log_dir/seed.stderr" || die "merge-base seed failed; inspect $log_dir/seed.stderr"

  # A fully local warm measurement may correctly have no provider session at
  # all. The first seed of a profile cannot: its empty store has
  # provider-eligible misses, so use it to prove that the configured S1/S2
  # namespace was actually reachable. Later seeds run against the warm shared
  # store, where an idle provider is legitimate, so the check runs once.
  if [[ "$require_remote" == "1" && ! -e "$sentinel" ]]; then
    (
      cd "$worktree"
      "$putnami_bin" sessions inspect latest --output=jsonl
    ) >"$log_dir/seed-session.json" 2>"$log_dir/seed-session.stderr" || die "merge-base seed created no inspectable session; inspect $log_dir/seed-session.stderr"
    jq -e '.metadata.cache.providerSummaryAvailable == true' "$log_dir/seed-session.json" >/dev/null || die "cache provider returned no Summary for the merge-base seed; inspect $log_dir/seed.stderr"
  fi
  : > "$sentinel"
}

# An S2 measurement never consumes the seed store — its local store starts
# empty so the trial restores from the CI cache. The only thing an S2 trial
# needs from seeding is the one-time provider verification, so seed at most
# once per profile, in a throwaway worktree.
ensure_profile_seeded() {
  local profile="$1"
  local change_class="$2"
  local log_dir="$3"
  local sentinel="$work_root/seed-verified-${profile}"
  if [[ -e "$sentinel" ]]; then
    return 0
  fi
  local verify_worktree="$work_root/seed-verify-${profile}"
  add_worktree "$verify_worktree"
  bootstrap_worktree "$verify_worktree" "$(seed_store_for_profile "$profile")" "$log_dir"
  seed_merge_base "$verify_worktree" "$profile" "$change_class" "$log_dir"
  remove_worktree "$verify_worktree"
}

capture_time_metrics() {
  local time_file="$1"
  local values
  values="$(awk '
    /real/ && /user/ && /sys/ {
      for (i = 1; i <= NF; i++) {
        if ($i == "real") real = $(i - 1)
        if ($i == "user") user = $(i - 1)
        if ($i == "sys") sys = $(i - 1)
      }
      if (real != "" && user != "" && sys != "") {
        printf "%.0f %.0f %.0f", real * 1000, user * 1000, sys * 1000
        exit
      }
    }
  ' "$time_file")"
  [[ -n "$values" ]] || die "could not parse /usr/bin/time -l output: $time_file"
  read -r wall_to_green_ms os_cpu_user_ms os_cpu_system_ms <<< "$values"
}

write_run_record() {
  local state="$1"
  local change_class="$2"
  local trial="$3"
  local session_file="$4"
  local session_recorded="$5"
  local log_dir="$6"
  local provider_summary_available=false

  if [[ "$session_recorded" == "true" ]]; then
    provider_summary_available="$(jq -r '.metadata.cache.providerSummaryAvailable // false' "$session_file")"
    # S0 and S2 both measure against stores with no local entries, so every
    # non-control cell in those states must exercise and summarize the
    # provider — an S2 measurement that never reached the provider is a cold
    # local recompute mislabeled as S2. Only S1 can legitimately be entirely
    # local after its verified seed; that zero-transfer record retains
    # available=false and bytesUp/bytesDown=0 rather than inventing a summary.
    if [[ ("$state" == "S0" || "$state" == "S2") && "$change_class" != "no-change" && "$require_remote" == "1" && "$provider_summary_available" != "true" ]]; then
      die "cache provider did not return its Summary result for ${state} (empty-store state); inspect $log_dir/measure.stderr"
    fi
  fi

  jq -cn \
    --arg recorded_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg date "$utc_date" \
    --arg machine "$machine" \
    --arg state "$state" \
    --arg change_class "$change_class" \
    --arg baseline "$baseline_sha" \
    --arg baseline_ref "$baseline_ref" \
    --arg command "${command_args[*]} ${selection_args[*]}" \
    --arg run_id "$run_id" \
    --argjson provider_summary_required "$require_remote" \
    --argjson trial "$trial" \
    --argjson wall_to_green_ms "$wall_to_green_ms" \
    --argjson os_cpu_user_ms "$os_cpu_user_ms" \
    --argjson os_cpu_system_ms "$os_cpu_system_ms" \
    --argjson session_recorded "$session_recorded" \
    --slurpfile session "$session_file" '
      ($session[0] // {}) as $inspection |
      ($inspection.metadata // {}) as $metadata |
      ($metadata.stats // {}) as $stats |
      ($metadata.cache // {}) as $cache |
      {
        schemaVersion: 1,
        recordType: "run",
        labels: {
          recordedAt: $recorded_at,
          date: $date,
          machine: $machine,
          cacheState: $state,
          changeClass: $change_class,
          baseline: $baseline,
          baselineRef: $baseline_ref,
          runID: $run_id,
          cacheProviderSummaryRequired: $provider_summary_required
        },
        command: $command,
        trial: $trial,
        session: {
          recorded: $session_recorded,
          id: ($metadata.id // null),
          stats: ($metadata.stats // null),
          jobs: ($metadata.jobs // []),
          scheduler: ($metadata.scheduler // null),
          cache: ($metadata.cache // null)
        },
        metrics: {
          wallToGreenMs: $wall_to_green_ms,
          osCpuUserMs: $os_cpu_user_ms,
          osCpuSystemMs: $os_cpu_system_ms,
          osCpuTotalMs: ($os_cpu_user_ms + $os_cpu_system_ms),
          summedJobWallMs: ($stats.durationMs // 0),
          cacheHitRate: (if (($stats.total // 0) > 0) then (($stats.cached // 0) / $stats.total) else 0 end),
          coalescedJobs: ($stats.coalesced // 0),
          coalescedRate: (if (($stats.total // 0) > 0) then (($stats.coalesced // 0) / $stats.total) else 0 end),
          taskEconomics: [
            ($metadata.jobs // [])[]
            | select(.spawnToFirstEventMs != null)
            | {
                taskKind,
                project,
                job,
                spawnToFirstEventMs,
                taskWallMs
              }
          ],
          cacheProviderSummary: {
            available: ($cache.providerSummaryAvailable // false),
            bytesDown: ($cache.providerSummaryRestoredBytes // 0),
            bytesUp: ($cache.providerSummaryUploadedBytes // 0),
            restoredCount: ($cache.providerSummaryRestoredCount // 0),
            uploadedCount: ($cache.providerSummaryUploadedCount // 0)
          }
        }
      }
    ' >> "$runs_file"
}

measure_trial() {
  local state="$1"
  local change_class="$2"
  local trial="$3"
  local profile
  profile="$(seed_profile_for_class "$change_class")"
  local trial_root="$work_root/${state}-${change_class}-${trial}"
  local measure_store_dir="$trial_root/measure-store"
  local log_dir="$output_dir/logs/${state}-${change_class}-${trial}"
  local measure_worktree="$trial_root/measure"
  local marker="${run_id}-${state}-${change_class}-${trial}"
  mkdir -p "$log_dir"
  configure_cache_access "$state" "$change_class" "$trial"

  case "$state" in
    S0)
      mkdir -p "$measure_store_dir"
      add_worktree "$measure_worktree"
      bootstrap_worktree "$measure_worktree" "$measure_store_dir" "$log_dir"
      ;;
    S1)
      # S1 measures a worktree that just completed a full merge-base seed,
      # using the profile's shared seed store as its local store.
      measure_store_dir="$(seed_store_for_profile "$profile")"
      add_worktree "$measure_worktree"
      bootstrap_worktree "$measure_worktree" "$measure_store_dir" "$log_dir"
      seed_merge_base "$measure_worktree" "$profile" "$change_class" "$log_dir"
      ;;
    S2)
      # S2 measures a fresh worktree with a deliberately empty local store so
      # it restores from the CI cache rather than inheriting local entries.
      ensure_profile_seeded "$profile" "$change_class" "$log_dir"
      mkdir -p "$measure_store_dir"
      add_worktree "$measure_worktree"
      bootstrap_worktree "$measure_worktree" "$measure_store_dir" "$log_dir"
      ;;
  esac

  apply_change "$measure_worktree" "$change_class" "$marker"
  selection_args_for_class "$change_class"

  if ! (
    cd "$measure_worktree"
    env PUTNAMI_STORE_DIR="$measure_store_dir" PUTNAMI_CACHE_URL="$cache_url" PUTNAMI_CACHE_TOKEN="$cache_token" \
      /usr/bin/time -l -o "$log_dir/time.txt" \
      "$putnami_bin" "${command_args[@]}" "${selection_args[@]}" --output=json
  ) >"$log_dir/measure.stdout" 2>"$log_dir/measure.stderr"; then
    die "measurement failed for ${state}/${change_class} trial ${trial}; inspect $log_dir/measure.stderr"
  fi
  capture_time_metrics "$log_dir/time.txt"

  local session_file="$log_dir/session.json"
  local session_recorded=true
  # A no-change control correctly selects zero projects. S1/S2 have a seed
  # session in the same worktree, so explicitly avoid treating that older seed
  # as if it were the measurement's session.
  if [[ "$change_class" == "no-change" ]]; then
    printf '%s\n' '{}' > "$session_file"
    session_recorded=false
  elif ! (
    cd "$measure_worktree"
    "$putnami_bin" sessions inspect latest --output=jsonl
  ) >"$session_file" 2>"$log_dir/session.stderr"; then
    die "measurement created no inspectable session for ${state}/${change_class}; inspect $log_dir/session.stderr"
  fi

  write_run_record "$state" "$change_class" "$trial" "$session_file" "$session_recorded" "$log_dir"
  remove_worktree "$measure_worktree"
  printf '%s\n' "${script_name}: completed ${state}/${change_class} trial ${trial}"
}

for state in "${states[@]}"; do
  for change_class in "${change_classes[@]}"; do
    for ((trial = 1; trial <= trials; trial++)); do
      measure_trial "$state" "$change_class" "$trial"
    done
  done
done

jq -s '
  def percentile($p):
    sort as $values |
    ($values | length) as $count |
    if $count == 0 then null else $values[((($count * $p) | ceil) - 1)] end;
  def summary($values): {
    p50: ($values | percentile(0.5)),
    p95: ($values | percentile(0.95))
  };
  def task_summaries($cohort):
    [
      $cohort[]
      | .metrics.taskEconomics[]?
      | select(.taskKind != null and .spawnToFirstEventMs != null)
    ]
    | sort_by(.taskKind)
    | group_by(.taskKind)
    | map(
        . as $tasks
        | {
            taskKind: $tasks[0].taskKind,
            samples: ($tasks | length),
            # Startup overhead and task wall remain separate summaries. They
            # must never be added together or rendered in the same value.
            spawnToFirstEventMs: ($tasks | map(.spawnToFirstEventMs) | summary(.)),
            taskWallMs: ($tasks | map(.taskWallMs) | summary(.))
          }
      );
  (map(select(.recordType == "run")) | sort_by(.labels.cacheState, .labels.changeClass)) as $runs |
  ($runs
    | group_by([.labels.cacheState, .labels.changeClass])
    | map(
        . as $cell |
        {
          cacheState: $cell[0].labels.cacheState,
          changeClass: $cell[0].labels.changeClass,
          trials: ($cell | length),
          wallToGreenMs: ($cell | map(.metrics.wallToGreenMs) | summary(.)),
          osCpuTotalMs: ($cell | map(.metrics.osCpuTotalMs) | summary(.)),
          summedJobWallMs: ($cell | map(.metrics.summedJobWallMs) | summary(.)),
          taskOverhead: task_summaries($cell),
          cacheHitRate: ($cell | map(.metrics.cacheHitRate) | summary(.)),
          coalescedJobs: ($cell | map(.metrics.coalescedJobs) | summary(.)),
          coalescedRate: ($cell | map(.metrics.coalescedRate) | summary(.)),
          providerBytesDown: ($cell | map(.metrics.cacheProviderSummary.bytesDown) | summary(.)),
          providerBytesUp: ($cell | map(.metrics.cacheProviderSummary.bytesUp) | summary(.))
        }
      )
  ) as $cells |
  def cell($state; $class):
    ($cells | map(select(.cacheState == $state and .changeClass == $class)) | first);
  {
    schemaVersion: 1,
    generatedAt: (now | todateiso8601),
    percentileMethod: "nearest-rank",
    cells: $cells,
    taskOverheadByKind: task_summaries($runs),
    comparisons: (
      [$cells[] | select(.cacheState == "S0") | .changeClass] | unique | map(
        . as $class |
        (cell("S0"; $class)) as $s0 |
        (cell("S1"; $class)) as $s1 |
        (cell("S2"; $class)) as $s2 |
        (if $s0 != null and $s1 != null and $s0.wallToGreenMs.p50 > 0
         then ((($s0.wallToGreenMs.p50 - $s1.wallToGreenMs.p50) * 100 / $s0.wallToGreenMs.p50) | round)
         else null end) as $savings |
        {
          changeClass: $class,
          ciSeededSavingsVsS0P50Percent: $savings,
          ciSeededSavingsTargetPercent: "50-80",
          # The 50-80% figure is a forecast band, not a pass window: the target
          # is met at >=50%, and savings above 80% exceed the forecast rather
          # than fail it.
          ciSeededSavingsTargetMet: (if $savings == null then null else $savings >= 50 end),
          ciSeededSavingsForecastBand:
            (if $savings == null then null
             elif $savings < 50 then "below"
             elif $savings <= 80 then "within"
             else "above" end),
          freshWorktreeS2P95Ms: (if $s2 == null then null else $s2.wallToGreenMs.p95 end),
          freshWorktreeSubMinuteTargetMet:
            (if $s2 == null then null else ($s2.wallToGreenMs.p95 < 60000) end)
        }
      )
    )
  }
' "$runs_file" > "$report_json"

{
  printf '%s\n' '# Cache economics benchmark report'
  printf '\n%s\n' "- Generated: $(jq -r '.generatedAt' "$report_json")"
  printf '%s\n' "- Baseline: \`${baseline_sha}\` (${baseline_ref})"
  printf '%s\n' "- Machine: ${machine}"
  printf '%s\n' '- Percentiles: nearest-rank p50/p95; every value stays in its named unit.'
  printf '\n%s\n' '## Per-cell results'
  printf '%s\n' '| State | Change class | N | Wall p50/p95 (ms) | CPU p50/p95 (ms) | Job wall p50/p95 (ms) | Startup overhead by task kind p50/p95 (ms) | Hit p50 (%) | Coalesced p50 | Down p50 (B) | Up p50 (B) |'
  printf '%s\n' '| --- | --- | ---: | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: |'
  jq -r '.cells[] | [
    .cacheState,
    .changeClass,
    .trials,
    "\(.wallToGreenMs.p50)/\(.wallToGreenMs.p95)",
    "\(.osCpuTotalMs.p50)/\(.osCpuTotalMs.p95)",
    "\(.summedJobWallMs.p50)/\(.summedJobWallMs.p95)",
    ([.taskOverhead[] | "\(.taskKind): \(.spawnToFirstEventMs.p50)/\(.spawnToFirstEventMs.p95)"] | if length == 0 then "n/a" else join("<br>") end),
    ((.cacheHitRate.p50 * 100) | round),
    .coalescedJobs.p50,
    .providerBytesDown.p50,
    .providerBytesUp.p50
  ] | @tsv' "$report_json" | while IFS=$'\t' read -r state class n wall cpu job overhead hit coalesced down up; do
    printf '| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n' "$state" "$class" "$n" "$wall" "$cpu" "$job" "$overhead" "$hit" "$coalesced" "$down" "$up"
  done
  printf '\n%s\n' '## Task startup overhead'
  printf '%s\n' '| Task kind | N | Spawn→first event p50/p95 (ms) | Task wall p50/p95 (ms) |'
  printf '%s\n' '| --- | ---: | ---: | ---: |'
  jq -r '.taskOverheadByKind[] | [
    .taskKind,
    .samples,
    "\(.spawnToFirstEventMs.p50)/\(.spawnToFirstEventMs.p95)",
    "\(.taskWallMs.p50)/\(.taskWallMs.p95)"
  ] | @tsv' "$report_json" | while IFS=$'\t' read -r task_kind n overhead wall; do
    printf '| %s | %s | %s | %s |\n' "$task_kind" "$n" "$overhead" "$wall"
  done
  printf '\n%s\n' '## Measured vs targets'
  printf '%s\n' '| Change class | S1 saving vs S0 p50 | Saving target (>=50%; forecast 50-80%) | S2 p95 (ms) | Fresh-worktree target (<60s) |'
  printf '%s\n' '| --- | ---: | --- | ---: | --- |'
  jq -r '.comparisons[] | [
    .changeClass,
    (if .ciSeededSavingsVsS0P50Percent == null then "n/a" else "\(.ciSeededSavingsVsS0P50Percent)%" end),
    (if .ciSeededSavingsTargetMet == null then "n/a"
     elif .ciSeededSavingsTargetMet | not then "not met"
     elif .ciSeededSavingsForecastBand == "above" then "met (above forecast)"
     else "met" end),
    (.freshWorktreeS2P95Ms // "n/a"),
    (if .freshWorktreeSubMinuteTargetMet == null then "n/a" elif .freshWorktreeSubMinuteTargetMet then "met" else "not met" end)
  ] | @tsv' "$report_json" | while IFS=$'\t' read -r class savings saving_target s2_p95 fresh_target; do
    printf '| %s | %s | %s | %s | %s |\n' "$class" "$savings" "$saving_target" "$s2_p95" "$fresh_target"
  done
} > "$report_md"

printf '%s\n' "${script_name}: wrote $runs_file"
printf '%s\n' "${script_name}: wrote $report_json"
printf '%s\n' "${script_name}: wrote $report_md"
