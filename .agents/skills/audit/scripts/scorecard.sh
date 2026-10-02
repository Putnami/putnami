#!/usr/bin/env bash
# Deterministic scorecard refresh for /audit.
#
# Renders every scope scorecard plus the workspace rollup from the open tasks
# of each scope — no LLM involved. Safe to run from /audit --scorecard-only,
# from fleet.sh's post-pass, or from a cron.
#
# Usage:
#   scorecard.sh [--scope <path>] [--narrative-file <file>] [--dry-run]
#
#   --scope <path>          only rewrite that scope's scorecard (the workspace
#                           rollup is still refreshed — it aggregates all scopes)
#   --narrative-file <file> add the file's text as the "Last wave" section of
#                           every rewritten scope scorecard
#   --dry-run               print the generated bodies and write nothing
#
# Scopes come from `putnami scopes list` and settings from
# options["@putnami/intelligence"].audit (see config.sh). Scorecards are opt-in
# (`"scorecards": true`). Each scorecard is a task of the bound tasks provider,
# found or opened through `putnami tasks create` with the idempotency key
# `scorecard:<scope label>` (`scorecard:workspace` for the rollup), so nothing
# records task numbers. The key carries the label because a provider compares a
# repeated create's title and labels: a new labelPrefix opens new scorecards
# instead of failing on the old key. The provider matches keys per account.
# Grade rubric: F (>=3 critical) · D (1-2 critical) · C (0C, >2 high) ·
# B (0C, 1-2 high) · A- (0C/0H, >2 medium) · A (otherwise).
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
HERE="$(cd "$(dirname "$0")" && pwd)"
CONFIG="$HERE/config.sh"
TODAY="$(date +%Y-%m-%d)"

DRY_RUN=0
ONLY_SCOPE=""
NARRATIVE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --scope) ONLY_SCOPE="${2:?--scope needs a value}"; shift ;;
    --narrative-file) NARRATIVE="$(cat "${2:?--narrative-file needs a file}")"; shift ;;
    *) echo "scorecard: unknown argument '$1'" >&2; exit 2 ;;
  esac
  shift
done

command -v jq >/dev/null || { echo "scorecard: jq not found" >&2; exit 1; }

if [ "$DRY_RUN" != 1 ] && ! bash "$CONFIG" scorecards; then
  echo "scorecard: scorecards are off; set options[\"@putnami/intelligence\"].audit.scorecards to true to enable them" >&2
  exit 0
fi

putnami_cli() {
  if [ -x "$ROOT/putnamiw" ]; then
    "$ROOT/putnamiw" "$@"
  else
    putnami "$@"
  fi
}

# tasks <operation> <input-json> prints the operation's result, or fails with
# the provider's envelope on stderr when the outcome is not ok.
tasks() {
  local out
  out="$(cd "$ROOT" && putnami_cli tasks "$1" --input "$2")" || { printf '%s\n' "$out" >&2; return 1; }
  if [ "$(printf '%s' "$out" | jq -r '.outcome')" != "ok" ]; then
    printf 'scorecard: tasks %s failed:\n%s\n' "$1" "$out" >&2
    return 1
  fi
  printf '%s' "$out" | jq -c '.result'
}

# open_tasks <label> prints every open task carrying the label as one JSON
# array of {title, labels}, following the provider's pages.
open_tasks() {
  local label="$1" cursor="" acc="[]" page input
  while :; do
    input="$(jq -nc --arg l "$label" --arg c "$cursor" \
      '{labels: [$l], states: ["open", "in_progress", "blocked"], page: ({size: 100} + (if $c == "" then {} else {cursor: $c} end))}')"
    # Explicit returns: errexit does not apply inside the caller's $(...).
    page="$(tasks find "$input")" || return 1
    acc="$(printf '%s' "$page" | jq -c --argjson acc "$acc" '$acc + [.items[] | {title, labels: (.labels // [])}]')" || return 1
    cursor="$(printf '%s' "$page" | jq -r '.page.next // empty')" || return 1
    [ -n "$cursor" ] || break
  done
  printf '%s' "$acc"
}

# scorecard_task <key> <title> <label...> finds or opens the scorecard task and
# prints its ref. The create input never changes for a key, so a repeated call
# answers the task the first call opened.
scorecard_task() {
  local key="$1" title="$2"; shift 2
  local labels
  labels="$(printf '%s\n' scorecard "$@" | jq -R . | jq -sc 'unique')"
  tasks create "$(jq -nc --arg k "$key" --arg t "$title" --argjson l "$labels" \
    '{idempotencyKey: $k, title: $t, labels: $l}')" | jq -c '.task.ref'
}

write_body() { # $1=ref-json $2=body
  tasks update "$(jq -nc --argjson ref "$1" --arg b "$2" '{ref: $ref, body: $b}')" >/dev/null
}

# jq library shared by the scope-body and summary renderers.
JQ_LIB='
def grade(c; h; m):
  if c >= 3 then "F"
  elif c >= 1 then "D"
  elif h > 2 then "C"
  elif h >= 1 then "B"
  elif m > 2 then "A-"
  else "A" end;

def counts:
  {
    c: ([.[] | select(.sev == "critical")] | length),
    h: ([.[] | select(.sev == "high")] | length),
    m: ([.[] | select(.sev == "medium")] | length),
    l: ([.[] | select(.sev == "low")] | length),
    total: length
  };

def cell_grade: counts | grade(.c; .h; .m);

def normalize:
  [ .[]
    | select((.labels | index("scorecard")) | not)
    | { project: ((.title | capture("^\\[(?<p>[^\\]]+)\\]").p?) // "(unscoped)"),
        sev: (([.labels[] | select(startswith("severity/"))] | first // "severity/medium")
              | ltrimstr("severity/")),
        group: (([.labels[] | select(startswith("group/"))] | first // "group/(none)")
                | ltrimstr("group/")) }
  ];
'

GROUPS_JSON='["security","performance","testing","design","operations","developer-experience"]'
COLS_JSON='{"security":"sec","performance":"perf","testing":"test","design":"dsgn","operations":"ops","developer-experience":"dx"}'

render_scope_body() { # $1=scope $2=narrative  stdin=open tasks json
  jq -r --arg scope "$1" --arg narrative "$2" --arg date "$TODAY" \
     --argjson groups "$GROUPS_JSON" --argjson cols "$COLS_JSON" "$JQ_LIB"'
  normalize as $issues
  | ($issues | counts) as $tot
  | ($issues | cell_grade) as $overall
  | ($issues | [.[].project] | unique | sort) as $projects
  | ([ "# `\($scope)` Scope Scorecard",
       "**Scope**: `\($scope)` | **Grade: \($overall)** | **Open issues: \($tot.total)** (\($tot.c) critical, \($tot.h) high, \($tot.m) medium, \($tot.l) low)",
       "_Last updated: \($date) — generated by `scorecard.sh` from open tasks._",
       "" ]
     + (if $tot.total == 0 then ["No open issues."] else
        [ "## Project × Group",
          "| Project | " + ([$groups[] | $cols[.]] | join(" | ")) + " | OVR |",
          "|---------|" + ([$groups[] | "-----"] | join("|")) + "|-----|" ]
        + [ $projects[] as $p
            | ($issues | map(select(.project == $p))) as $pi
            | "| `\($p)` | "
              + ([$groups[] as $g
                  | ($pi | map(select(.group == $g)))
                  | if length == 0 then "-" else cell_grade end] | join(" | "))
              + " | **\($pi | cell_grade)** |" ]
        + [ "",
            "## By Group",
            "| Group | Grade | C | H | M | L | Total |",
            "|-------|-------|---|---|---|---|-------|" ]
        + [ $groups[] as $g
            | ($issues | map(select(.group == $g)) | counts) as $gc
            | "| \($g) | \(if $gc.total == 0 then "-" else ($issues | map(select(.group == $g)) | cell_grade) end) | \($gc.c) | \($gc.h) | \($gc.m) | \($gc.l) | \($gc.total) |" ]
        end)
     + (if $narrative == "" then [] else ["", "## Last wave", "", $narrative] end))
  | join("\n")'
}

scope_summary() { # $1=scope  stdin=open tasks json
  jq -c --arg scope "$1" "$JQ_LIB"'
  normalize
  | (counts) as $tot
  | { scope: $scope, grade: cell_grade,
      c: $tot.c, h: $tot.h, m: $tot.m, l: $tot.l, total: $tot.total }'
}

scopes="$(bash "$CONFIG" scopes)"
if [ -n "$ONLY_SCOPE" ] && ! printf '%s\n' "$scopes" | jq -e --arg s "$ONLY_SCOPE" 'select(.scope == $s)' >/dev/null; then
  echo "scorecard: unknown scope '$ONLY_SCOPE' (not in \`putnami scopes list\`)" >&2
  exit 2
fi

summaries="[]"
while IFS= read -r row; do
  [ -n "$row" ] || continue
  scope="$(printf '%s' "$row" | jq -r '.scope')"
  label="$(printf '%s' "$row" | jq -r '.label')"

  # A failed read stops the refresh: an empty list would overwrite the
  # scorecard with a clean grade.
  raw="$(open_tasks "$label")" || { echo "scorecard: cannot read the open tasks of $scope" >&2; exit 1; }
  summaries="$(printf '%s' "$raw" | scope_summary "$scope" | jq -c --argjson acc "$summaries" '$acc + [.]')"

  if [ -n "$ONLY_SCOPE" ] && [ "$scope" != "$ONLY_SCOPE" ]; then
    continue
  fi

  body="$(printf '%s' "$raw" | render_scope_body "$scope" "$NARRATIVE")"
  if [ "$DRY_RUN" = 1 ]; then
    printf -- '--- scorecard:%s ---\n%s\n\n' "$scope" "$body"
  else
    ref="$(scorecard_task "scorecard:$label" "[$scope] Scope scorecard" "$label")"
    write_body "$ref" "$body"
    echo "scorecard: updated $scope ($(printf '%s' "$ref" | jq -r '.id'))"
  fi
done <<< "$scopes"

ws_body="$(printf '%s' "$summaries" | jq -r --arg date "$TODAY" '
def grade_rank: {"A": 0, "A-": 1, "B": 2, "C": 3, "D": 4, "F": 5}[.];
. as $rows
| (if ($rows | length) == 0 then "A" else ([$rows[].grade] | max_by(grade_rank)) end) as $worst
| ([ "# Workspace Scorecard",
     "**Workspace grade: \($worst)** (worst scope) | _Last updated: \($date) — generated by `scorecard.sh`_",
     "",
     "## Scope Rollup",
     "| Scope | Grade | Critical | High | Medium | Low | Open |",
     "|-------|-------|----------|------|--------|-----|------|" ]
   + [ $rows[] | "| `\(.scope)` | \(.grade) | \(.c) | \(.h) | \(.m) | \(.l) | \(.total) |" ]
   + [ "",
       "## Notes",
       "- Grade rubric: **F** (≥3 critical) · **D** (1-2 critical) · **C** (0C, >2 high) · **B** (0C, 1-2 high) · **A-** (0C/0H, >2 medium) · **A** (otherwise).",
       "- Scope scorecards carry the per-project × group detail and the last wave narrative.",
       "- Refresh anytime with the audit skill'"'"'s `scorecard.sh` (no model, no scan)." ])
| join("\n")')"
if [ "$DRY_RUN" = 1 ]; then
  printf -- '--- scorecard:workspace ---\n%s\n' "$ws_body"
else
  ref="$(scorecard_task "scorecard:workspace" "[workspace] Workspace scorecard")"
  write_body "$ref" "$ws_body"
  echo "scorecard: updated workspace ($(printf '%s' "$ref" | jq -r '.id'))"
fi
