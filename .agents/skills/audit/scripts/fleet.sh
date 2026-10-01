#!/usr/bin/env bash
# Parallel audit shard orchestrator for Claude Code and Codex.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"
DOMAINS_SH="$SCRIPT_DIR/domains.sh"
SCORECARD_SH="$SCRIPT_DIR/scorecard.sh"
# GROUPS is a readonly bash builtin. The order is the workspace-wide group
# priority from SKILL.md, so higher-priority shards launch first.
GROUP_LIST="security testing design performance developer-experience operations"
# A domain with at most this many projects runs as one shard for all groups;
# a larger domain runs one shard per group.
COMBINED_MAX_PROJECTS=6

MAX_PARALLEL=4
DRY_RUN=0
FILTER_DOMAIN=""
FILTER_GROUP=""
SKIP_SCORECARD_PASS=0
NO_CACHE=""
HOST="${PUTNAMI_AGENT_HOST:-auto}"
CODEX_BIN="${PUTNAMI_CODEX_BIN:-}"

usage() {
  cat <<'USAGE'
Usage:
  fleet.sh [--host claude|codex|auto] [--dry-run] [--domain <domain>]
           [--group <group>] [--max-parallel <n>] [--no-cache]
           [--skip-scorecard-pass]

PUTNAMI_CODEX_BIN may point at a specific Codex binary. Codex runs require an
installed catalog containing exact gpt-6-astra; the fleet fails rather than
silently selecting an older model.
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --host) HOST="${2:?--host needs a value}"; shift ;;
    --dry-run) DRY_RUN=1 ;;
    --domain) FILTER_DOMAIN="${2:?--domain needs a value}"; shift ;;
    --group) FILTER_GROUP="${2:?--group needs a value}"; shift ;;
    --max-parallel) MAX_PARALLEL="${2:?--max-parallel needs a value}"; shift ;;
    --no-cache) NO_CACHE=" --no-cache" ;;
    --skip-scorecard-pass) SKIP_SCORECARD_PASS=1 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "fleet: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

case "$MAX_PARALLEL" in
  *[!0-9]*|"") echo "fleet: --max-parallel must be a positive integer" >&2; exit 2 ;;
esac
[ "$MAX_PARALLEL" -gt 0 ] || { echo "fleet: --max-parallel must be greater than zero" >&2; exit 2; }

if [ "$HOST" = "auto" ]; then
  if [ -n "${CODEX_THREAD_ID:-}" ]; then
    HOST="codex"
  elif [ -n "${CLAUDE_CODE_ENTRYPOINT:-}" ]; then
    HOST="claude"
  elif command -v claude >/dev/null; then
    HOST="claude"
  else
    HOST="codex"
  fi
fi
case "$HOST" in
  claude|codex) ;;
  *) echo "fleet: --host must be claude, codex, or auto" >&2; exit 2 ;;
esac

command -v jq >/dev/null || { echo "fleet: jq not found" >&2; exit 1; }

codex_has_sol() {
  local candidate="$1"
  [ -x "$candidate" ] || return 1
  "$candidate" debug models 2>/dev/null |
    jq -e '.. | strings | select(. == "gpt-6-astra")' >/dev/null
}

resolve_codex() {
  local candidate=""
  if [ -n "$CODEX_BIN" ]; then
    codex_has_sol "$CODEX_BIN" || {
      echo "fleet: PUTNAMI_CODEX_BIN does not expose gpt-6-astra: $CODEX_BIN" >&2
      return 1
    }
    return 0
  fi

  candidate="$(command -v codex 2>/dev/null || true)"
  if [ -n "$candidate" ] && codex_has_sol "$candidate"; then
    CODEX_BIN="$candidate"
    return 0
  fi

  echo "fleet: no Codex CLI exposing exact gpt-6-astra was found" >&2
  echo "fleet: update Codex or set PUTNAMI_CODEX_BIN to a Codex CLI that exposes it" >&2
  return 1
}

if [ "$DRY_RUN" = 0 ]; then
  if [ "$HOST" = "claude" ]; then
    command -v claude >/dev/null || { echo "fleet: claude CLI not found" >&2; exit 1; }
  else
    resolve_codex
  fi
fi

# Entries are domain|group; an empty group means a combined domain shard.
DOMAINS_JSON="$(bash "$DOMAINS_SH")"
SLICES=()
for domain in $(printf '%s' "$DOMAINS_JSON" | jq -r 'keys_unsorted[]'); do
  if [ -n "$FILTER_DOMAIN" ] && [ "$domain" != "$FILTER_DOMAIN" ]; then
    continue
  fi
  if [ -n "$FILTER_GROUP" ]; then
    SLICES+=("$domain|$FILTER_GROUP")
    continue
  fi
  project_count="$(printf '%s' "$DOMAINS_JSON" | jq -r --arg domain "$domain" '.[$domain] | length')"
  if [ "$project_count" -le "$COMBINED_MAX_PROJECTS" ]; then
    SLICES+=("$domain|")
  else
    for group in $GROUP_LIST; do
      SLICES+=("$domain|$group")
    done
  fi
done

if [ "${#SLICES[@]}" -eq 0 ]; then
  echo "fleet: no slices matched" >&2
  exit 1
fi

if [ "$DRY_RUN" = 1 ]; then
  echo "fleet: host=$HOST, ${#SLICES[@]} slice(s), max $MAX_PARALLEL concurrent:"
  for slice in "${SLICES[@]}"; do
    domain="${slice%%|*}"
    group="${slice#*|}"
    projects="$(printf '%s' "$DOMAINS_JSON" | jq -r --arg domain "$domain" '.[$domain] | length')"
    if [ -n "$group" ]; then
      echo "  audit --domain $domain --group $group --skip-scorecard$NO_CACHE  # $projects projects"
    else
      echo "  audit --domain $domain --skip-scorecard$NO_CACHE  # $projects projects"
    fi
  done
  [ "$SKIP_SCORECARD_PASS" = 1 ] || echo "  + final: scorecard.sh"
  exit 0
fi

RUN_DIR=".audit-runs/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RUN_DIR"
echo "fleet: host=$HOST, ${#SLICES[@]} slice(s), max $MAX_PARALLEL concurrent, logs in $RUN_DIR"

PIDS=()
NAMES=()
for slice in "${SLICES[@]}"; do
  domain="${slice%%|*}"
  group="${slice#*|}"
  name="$domain"
  args="--domain $domain"
  if [ -n "$group" ]; then
    name="$domain-$group"
    args="$args --group $group"
  fi
  while [ "$(jobs -pr | wc -l | tr -d ' ')" -ge "$MAX_PARALLEL" ]; do
    sleep 5
  done
  echo "fleet: launching $name"
  if [ "$HOST" = "claude" ]; then
    claude -p "/audit $args --skip-scorecard$NO_CACHE" \
      --permission-mode bypassPermissions \
      >"$RUN_DIR/$name.out" 2>"$RUN_DIR/$name.err" &
  else
    "$CODEX_BIN" exec -C "$ROOT" \
      --model gpt-6-astra \
      --config 'model_reasoning_effort="xhigh"' \
      --dangerously-bypass-approvals-and-sandbox \
      "Use the audit skill with these exact arguments: $args --skip-scorecard$NO_CACHE" \
      >"$RUN_DIR/$name.out" 2>"$RUN_DIR/$name.err" &
  fi
  PIDS+=("$!")
  NAMES+=("$name")
done

FAILED=0
: >"$RUN_DIR/summary.txt"
for index in "${!PIDS[@]}"; do
  if wait "${PIDS[$index]}"; then
    echo "ok      ${NAMES[$index]}" >>"$RUN_DIR/summary.txt"
  else
    echo "FAILED  ${NAMES[$index]}  (see $RUN_DIR/${NAMES[$index]}.err)" >>"$RUN_DIR/summary.txt"
    FAILED=$((FAILED + 1))
  fi
done

cat "$RUN_DIR/summary.txt"

if [ "$SKIP_SCORECARD_PASS" = 0 ]; then
  echo "fleet: refreshing scorecards"
  bash "$SCORECARD_SH"
fi

if [ "$FAILED" -gt 0 ]; then
  echo "fleet: $FAILED slice(s) failed" >&2
  exit 1
fi
echo "fleet: done"
