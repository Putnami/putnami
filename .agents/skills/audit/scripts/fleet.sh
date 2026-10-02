#!/usr/bin/env bash
# Parallel /audit orchestrator for Putnami projects.
#
# Splits a full-workspace audit into (scope) or (scope, group) shards derived
# from `putnami scopes list` (through config.sh), runs them as backgrounded
# `claude -p` sessions with a concurrency cap, then runs scorecard.sh once —
# a plain deterministic script, no model — to refresh shared scorecard state.
# Attestations (attest.sh) make re-runs cheap: shards skip (project, group)
# pairs whose content hasn't changed; pass --no-cache to force a full re-scan.
# When the root manifest enables Intelligence, the ledger is prefetched from the
# shared Intelligence tier in one batched call before dispatch and seeded
# locally, so shards resolve locally instead of each making per-key remote
# lookups (fail-open; skipped under --no-cache).
#
# Slicing rule (mirrors SKILL.md "Parallel runs"):
#   - scope with <= 4 projects  -> one combined shard   (--scope s --skip-scorecard)
#   - scope with  > 4 projects  -> one shard per group  (--scope s --group g --skip-scorecard)
#
# Usage:
#   fleet.sh [--dry-run] [--scope <s>] [--group <g>] [--max-parallel <n>] [--no-cache] [--skip-scorecard-pass]
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_SH="$HERE/config.sh"
SCORECARD_SH="$HERE/scorecard.sh"
ATTEST_SH="$HERE/attest.sh"

DRY_RUN=0
ONLY_SCOPE=""
ONLY_GROUP=""
MAX_PARALLEL=4
SKIP_SCORECARD_PASS=0
NO_CACHE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --scope) ONLY_SCOPE="$2"; shift 2 ;;
    --group) ONLY_GROUP="$2"; shift 2 ;;
    --max-parallel) MAX_PARALLEL="$2"; shift 2 ;;
    --no-cache) NO_CACHE=" --no-cache"; shift ;;
    --skip-scorecard-pass) SKIP_SCORECARD_PASS=1; shift ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

# One JSON row per scope: {scope, label, projects, priority}. A failure stops
# the fleet: an empty scope set would read as coverage.
SCOPES="$(bash "$CONFIG_SH" scopes)"

# Build the slice list: "<scope>\t<group-or-empty>" (bash 3.2 compatible: no mapfile)
SLICES=()
while IFS= read -r row; do
  [[ -z "$row" ]] && continue
  s="$(jq -r '.scope' <<< "$row")"
  [[ -n "$ONLY_SCOPE" && "$s" != "$ONLY_SCOPE" ]] && continue
  count="$(jq -r '.projects | length' <<< "$row")"
  if [[ "${count:-0}" -gt 4 ]]; then
    while IFS= read -r g; do
      [[ -z "$g" ]] && continue
      [[ -n "$ONLY_GROUP" && "$g" != "$ONLY_GROUP" ]] && continue
      SLICES+=("$s	$g")
    done < <(jq -r '.priority[]' <<< "$row")
  else
    if [[ -n "$ONLY_GROUP" ]]; then
      SLICES+=("$s	$ONLY_GROUP")
    else
      SLICES+=("$s	")
    fi
  fi
done <<< "$SCOPES"

if [[ ${#SLICES[@]} -eq 0 ]]; then
  echo "No slices matched (scope=$ONLY_SCOPE group=$ONLY_GROUP)." >&2
  exit 1
fi

echo "Matrix (${#SLICES[@]} slices, max-parallel=$MAX_PARALLEL):"
for sl in "${SLICES[@]}"; do
  s="${sl%%	*}"; g="${sl#*	}"
  if [[ -n "$g" ]]; then echo "  /audit --scope $s --group $g --skip-scorecard$NO_CACHE"; else echo "  /audit --scope $s --skip-scorecard$NO_CACHE"; fi
done

if [[ "$DRY_RUN" -eq 1 ]]; then exit 0; fi

TS="$(date +%Y%m%d-%H%M%S)"
RUN_DIR="$REPO_ROOT/.putnami/audit/runs/$TS"
mkdir -p "$RUN_DIR"
echo "Writing shard output to $RUN_DIR"

run_slice() {
  local s="$1" g="$2"
  local name args rc=0
  if [[ -n "$g" ]]; then name="${s//\//-}-$g"; args="--scope $s --group $g --skip-scorecard$NO_CACHE"; else name="${s//\//-}"; args="--scope $s --skip-scorecard$NO_CACHE"; fi
  # `|| rc=$?` keeps the inherited errexit from ending the subshell before a
  # failed shard is recorded.
  claude -p "/audit $args" --permission-mode bypassPermissions \
    >"$RUN_DIR/$name.stdout" 2>"$RUN_DIR/$name.stderr" || rc=$?
  echo "$name	$rc" >>"$RUN_DIR/exit-status.tsv"
}

# --- shared-tier prefetch (opt-in; fail-open) ---------------------------------
prefetch_call() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "${PUTNAMI_AUDIT_ATTEST_TIMEOUT:-30}" putnami cloud audit-attest prefetch
  else
    putnami cloud audit-attest prefetch
  fi
}

# prefetch_ledger pulls the whole wave's ledger from shared Intelligence
# tier in ONE batched call and seeds the machine-local store, so shards resolve
# locally instead of each doing per-(project, group) remote round trips. Opt-in via
# the manifest Intelligence setting and fail-open (a `set +e` subshell): any error
# just means shards fall back to their own per-key lookups. Skipped under
# --no-cache. repo and rubricHash come from attest.sh so the derived keys match
# exactly what the remote tier uses.
prefetch_ledger() ( set +e
  bash "$CONFIG_SH" shared-attestations || exit 0
  [ -z "$NO_CACHE" ] || exit 0
  repo="$(bash "$ATTEST_SH" repo 2>/dev/null)"; [ -n "$repo" ] || exit 0
  rubric="$(bash "$ATTEST_SH" rubric 2>/dev/null)"; [ -n "$rubric" ] || exit 0

  # Build one JSONL line per (in-scope project, group). Skip dirty/no-tree
  # projects — they can never attest, so a lookup would always miss.
  batch=""
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    s="$(jq -r '.scope' <<< "$row")"
    [ -z "$ONLY_SCOPE" ] || [ "$s" = "$ONLY_SCOPE" ] || continue
    groups="$(jq -r '.priority[]' <<< "$row")"
    for p in $(jq -r '.projects[]' <<< "$row"); do
      # project-short comes from config.sh, the same source the audit shard
      # uses for `attest.sh check/record` (SKILL.md step 4b); a mismatch would
      # land the seed under a filename the shard never reads (a missed
      # prefetch — fail-safe, never a false skip).
      short="$(jq -r --arg p "$p" '.shorts[$p]' <<< "$row")"
      ih="$(bash "$ATTEST_SH" hash "$p" 2>/dev/null)"
      case "$ih" in ""|*-dirty|no-tree*) continue ;; esac
      for g in $groups; do
        [ -z "$ONLY_GROUP" ] || [ "$g" = "$ONLY_GROUP" ] || continue
        line="$(jq -cn \
          --arg label "$(printf '%s\t%s\t%s' "$short" "$g" "$ih")" \
          --arg repo "$repo" --arg project "$short" --arg group "$g" \
          --arg ih "$ih" --arg rubric "$rubric" \
          '{label:$label,repo:$repo,project:$project,group:$group,inputHash:$ih,rubricHash:$rubric}')"
        batch="${batch}${line}"$'\n'
      done
    done
  done <<< "$SCOPES"
  [ -n "$batch" ] || exit 0

  out="$(printf '%s' "$batch" | prefetch_call 2>/dev/null)"; [ -n "$out" ] || exit 0

  # Seed local hits only (a miss must never be seeded — that would be a false
  # skip). Remote is disabled for the seeding record so it does not re-mirror.
  seeded=0
  tab="$(printf '\t')"
  while IFS= read -r rl; do
    [ -n "$rl" ] || continue
    [ "$(printf '%s' "$rl" | jq -r '.found // false' 2>/dev/null)" = "true" ] || continue
    lbl="$(printf '%s' "$rl" | jq -r '.label' 2>/dev/null)"
    IFS="$tab" read -r sshort sgroup shash <<< "$lbl"
    [ -n "$sshort" ] && [ -n "$sgroup" ] && [ -n "$shash" ] || continue
    if bash "$ATTEST_SH" seed "$sshort" "$sgroup" "$shash" 2>/dev/null; then
      seeded=$((seeded + 1))
    fi
  done <<< "$out"
  echo "Prefetch: seeded $seeded local attestation(s) from the shared tier."
  exit 0
)

prefetch_ledger || true
# ------------------------------------------------------------------------------

# Fan out with a concurrency cap (bash 3.2 compatible: no `wait -n`).
# Track PIDs; once the batch is full, wait for the oldest before launching more.
PIDS=()
for sl in "${SLICES[@]}"; do
  s="${sl%%	*}"; g="${sl#*	}"
  run_slice "$s" "$g" &
  PIDS+=("$!")
  if [[ "${#PIDS[@]}" -ge "$MAX_PARALLEL" ]]; then
    wait "${PIDS[0]}" 2>/dev/null || true
    PIDS=("${PIDS[@]:1}")
  fi
done
wait

# Deterministic scorecard refresh (shared write surface) — a plain script, no model.
if [[ "$SKIP_SCORECARD_PASS" -eq 0 ]]; then
  echo "Refreshing scorecards (scorecard.sh)..."
  only=""
  [[ -n "$ONLY_SCOPE" ]] && only="--scope $ONLY_SCOPE"
  bash "$SCORECARD_SH" $only \
    >"$RUN_DIR/scorecard.stdout" 2>"$RUN_DIR/scorecard.stderr" || true
fi

{
  echo "Audit fleet run $TS"
  echo "Slices: ${#SLICES[@]} | max-parallel: $MAX_PARALLEL"
  echo
  echo "Per-slice exit status:"
  if [[ -f "$RUN_DIR/exit-status.tsv" ]]; then sort "$RUN_DIR/exit-status.tsv" | awk -F'\t' '{printf "  %-30s exit %s\n", $1, $2}'; fi
  echo
  echo "Failed shards (see <name>.stderr):"
  if [[ -f "$RUN_DIR/exit-status.tsv" ]]; then awk -F'\t' '$2!=0 {print "  "$1}' "$RUN_DIR/exit-status.tsv"; fi
} | tee "$RUN_DIR/summary.txt"
