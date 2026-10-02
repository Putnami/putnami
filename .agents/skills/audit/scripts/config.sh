#!/usr/bin/env bash
# Audit configuration resolver.
#
# The audit unit is a Putnami scope: its projects come from
# `putnami scopes list`, never from a hand-kept file. Repository settings live
# in the workspace manifest under options["@putnami/intelligence"].audit:
#
#   "@putnami/intelligence": {
#     "audit": {
#       "priority":    ["security", "operations", ...],  // workspace default order
#       "scopes":      { "<scope>": { "priority": [...] } },
#       "labelPrefix": "scope/",                           // scope label is <prefix><scope>
#       "scorecards":  true,                               // opt in to scorecard tasks
#       "profile":     ".AI/audit/profile.md",             // repository rubric, read before scanning
#       "checks":      "tools/audit/checks.sh"             // repository mechanical checks
#     }
#   }
#
# Every key is optional. Scope options cannot carry these settings: a scope
# putnami.json accepts `options` only when the scope is also a project.
#
# Usage:
#   config.sh scopes                 # JSONL: {"scope","label","projects":["/<path>",...],"shorts":{"/<path>":"<short>"},"priority":[...]}
#   config.sh priority <scope>       # property groups in priority order, one per line
#   config.sh label <scope>          # the scope's issue label
#   config.sh scope-of <project-id>  # the scope that owns a project id (exit 1 when none, 2 when the CLI fails)
#   config.sh short <project-id>     # the project's short name (exit 1 when unscoped, 2 when the CLI fails)
#   config.sh profile                # the repository profile path, relative to the root (exit 1 when none is set
#                                    # and the default is absent, 2 when the configured file is missing)
#   config.sh checks                 # the repository bash checks script, relative to the root (exit 1 when none is
#                                    # set, 2 when the configured file is missing)
#   config.sh modules                # the workspace's own module prefixes, one per line: the
#                                    # vanity host of each Go module path (host/owner on a
#                                    # shared code host, the whole path when it has one
#                                    # element) and each npm scope (@scope)
#   config.sh scorecards             # exit 0 when scorecard tasks are enabled
#   config.sh shared-attestations    # exit 0 when the shared Intelligence attestation tier is on
#
# Generated projects (tag `generated` or `generated-client`) are not audited.
# A project's short name is <scope>/<leaf directory>, or <scope>/<path under
# the scope> when two audited projects of the scope share a leaf.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
DEFAULT_PRIORITY='["security","operations","testing","design","performance","developer-experience"]'
DEFAULT_PROFILE='.AI/audit/profile.md'

manifest() {
  if [ -f "$ROOT/putnami.workspace.json" ]; then
    printf '%s' "$ROOT/putnami.workspace.json"
  elif [ -f "$ROOT/putnami.json" ]; then
    printf '%s' "$ROOT/putnami.json"
  else
    echo "config: no putnami.workspace.json or putnami.json at $ROOT" >&2
    return 1
  fi
}

audit_options() {
  jq -c '.options["@putnami/intelligence"].audit // {}' "$(manifest)"
}

putnami_cli() {
  if [ -x "$ROOT/putnamiw" ]; then
    "$ROOT/putnamiw" "$@"
  else
    putnami "$@"
  fi
}

# scopes_json prints the workspace scopes as one JSON array of
# {path, projects, shorts}, where each project is its id ("/<path>") and shorts
# maps an id to its short name. `scopes list` names projects, and a generated
# client's name is its module path, not its directory, so names are mapped to
# ids through `projects list`, which also carries the tags that exclude
# generated projects. A failed listing stops the audit: an empty scope set
# would read as coverage.
scopes_json() {
  local scopes projects
  if ! scopes="$(cd "$ROOT" && putnami_cli scopes list --output=json 2>/dev/null)" ||
    ! projects="$(cd "$ROOT" && putnami_cli projects list --output=json 2>/dev/null)"; then
    echo "config: \`putnami scopes list\` or \`putnami projects list\` failed; the audit cannot resolve its scopes" >&2
    return 1
  fi
  jq -cn --argjson s "$scopes" --argjson p "$projects" '
    ($p.data // [] | map({key: .name, value: .id}) | from_entries) as $ids
    | ($p.data // [] | map(select((.tags // []) | any(. == "generated" or . == "generated-client")) | .id)) as $generated
    | [($s.data // [])[]
       | .path as $scope
       | [.projects[] | $ids[.] // ("/" + .) | select(. as $id | $generated | index($id) | not)] as $projects
       | ($projects | map(split("/") | last)) as $leaves
       | {path: $scope,
          projects: $projects,
          shorts: ($projects | map({key: ., value: (
            (split("/") | last) as $leaf
            | if ([$leaves[] | select(. == $leaf)] | length) > 1
              then $scope + "/" + (ltrimstr("/") | ltrimstr($scope + "/"))
              else $scope + "/" + $leaf end)}) | from_entries)}]'
}

cmd="${1:-}"
[ -n "$cmd" ] || { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
shift

command -v jq >/dev/null || { echo "config: jq not found" >&2; exit 2; }

case "$cmd" in
  scopes)
    opts="$(audit_options)"
    scopes_json | jq -c --argjson opts "$opts" --argjson def "$DEFAULT_PRIORITY" '
      .[] | {
        scope: .path,
        label: (($opts.labelPrefix // "scope/") + .path),
        projects: .projects,
        shorts: .shorts,
        priority: ($opts.scopes[.path].priority // $opts.priority // $def)
      }'
    ;;
  priority)
    scope="${1:?usage: config.sh priority <scope>}"
    audit_options | jq -r --arg s "$scope" --argjson def "$DEFAULT_PRIORITY" \
      '(.scopes[$s].priority // .priority // $def)[]'
    ;;
  label)
    scope="${1:?usage: config.sh label <scope>}"
    audit_options | jq -r --arg s "$scope" '(.labelPrefix // "scope/") + $s'
    ;;
  scope-of)
    project="${1:?usage: config.sh scope-of <project-id>}"
    project="/${project#/}"
    all="$(scopes_json)" || exit 2
    found="$(printf '%s' "$all" | jq -r --arg p "$project" \
      '[.[] | select(.projects | index($p))] | first | .path // empty')"
    [ -n "$found" ] || exit 1
    printf '%s\n' "$found"
    ;;
  short)
    project="${1:?usage: config.sh short <project-id>}"
    project="/${project#/}"
    all="$(scopes_json)" || exit 2
    found="$(printf '%s' "$all" | jq -r --arg p "$project" \
      '[.[] | .shorts[$p] // empty] | first // empty')"
    [ -n "$found" ] || exit 1
    printf '%s\n' "$found"
    ;;
  scorecards)
    audit_options | jq -e '.scorecards == true' >/dev/null
    ;;
  profile)
    profile="$(audit_options | jq -r '.profile // empty')"
    if [ -z "$profile" ]; then
      [ -f "$ROOT/$DEFAULT_PROFILE" ] || exit 1
      profile="$DEFAULT_PROFILE"
    fi
    [ -f "$ROOT/$profile" ] || { echo "config: the audit profile $profile does not exist" >&2; exit 2; }
    printf '%s\n' "$profile"
    ;;
  modules)
    # Samples, templates and fixtures follow the workspace and publish nothing.
    cd "$ROOT"
    git ls-files -- 'go.mod' '*/go.mod' | grep -vE '(^|/)(testdata|fixtures|samples|templates)/' \
      | while read -r gm; do awk '/^module /{print $2; exit}' "$gm"; done \
      | awk -F/ 'NF == 1 { print $1 } NF > 1 { if ($1 ~ /^(github\.com|gitlab\.com|bitbucket\.org)$/) { if (NF > 2) print $1 "/" $2 } else print $1 }' \
      | sort -u || true
    git ls-files -- 'package.json' '*/package.json' | grep -vE '(^|/)(node_modules|testdata|fixtures|samples|templates)/' \
      | while read -r pj; do jq -r '.name // empty' "$pj" 2>/dev/null; done \
      | awk -F/ '/^@/ && NF == 2 { print $1 }' | sort -u || true
    ;;
  checks)
    checks="$(audit_options | jq -r '.checks // empty')"
    [ -n "$checks" ] || exit 1
    [ -f "$ROOT/$checks" ] || { echo "config: the audit checks script $checks does not exist" >&2; exit 2; }
    printf '%s\n' "$checks"
    ;;
  shared-attestations)
    jq -e '.options["@putnami/cloud"].intelligence.enabled == true' "$(manifest)" >/dev/null 2>&1
    ;;
  *)
    echo "config: unknown command '$cmd'" >&2
    exit 2
    ;;
esac
