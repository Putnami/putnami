#!/usr/bin/env bash
# dispatch.sh <request-file>
#
# The dispatch of a person's Putnami routine. The file holds the request of
# the routine's routine-fire-payload block: the JSON object alone, or the
# whole fire text, whose last non-empty line is that object. The script prints
# the skill to run and exits 0 only when the skill the request names is on this
# routine's allowlist: a build request runs routine-build, the build skill's
# routine entry point, and any other allowed skill runs under its own name.
# Otherwise it prints why on standard error and exits 1, and the routine runs
# nothing.
#
# A refused request that still carries a report address and token has the
# refusal reported there as failed, so Putnami stops waiting for it at once instead
# of waiting for the request's deadline.
#
# The allowlist ships with this skill and only a new version of the skill
# widens it: the fire text is untrusted data and never does.
set -euo pipefail

ALLOWLIST=(rebase build)

# skill_to_run prints the skill the routine runs for an allowed request.
skill_to_run() {
  case "$1" in
    build) printf 'routine-build\n' ;;
    *) printf '%s\n' "$1" ;;
  esac
}

line=""

# report_refusal posts {"status":"failed"} to the request's report address,
# only an https URL on a /v1/.../report path with a token of plain characters.
# The URL and the token reach curl as arguments, never through a shell.
report_refusal() {
  [[ -n "$line" ]] || return 0
  command -v curl >/dev/null 2>&1 || return 0
  local url token
  url=$(printf '%s' "$line" | jq -er 'objects | .report.url | strings' 2>/dev/null) || return 0
  token=$(printf '%s' "$line" | jq -er 'objects | .report.token | strings' 2>/dev/null) || return 0
  [[ "$url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?/v1/[a-z0-9/-]+/report$ ]] || return 0
  # The length is checked apart: macOS bash refuses a {8,512} bound, above 255.
  [[ "$token" =~ ^[A-Za-z0-9_.:-]+$ ]] && (( ${#token} >= 8 && ${#token} <= 512 )) || return 0
  if curl -sS --fail --max-time 20 --proto '=https' -X POST "$url" \
    -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
    --data '{"status":"failed","reason":"capability_missing"}' >/dev/null 2>&1; then
    printf 'putnami-routine: the refusal was reported to Putnami\n' >&2
  else
    printf 'putnami-routine: the refusal could not be reported\n' >&2
  fi
}

refuse() {
  printf 'putnami-routine: %s\n' "$1" >&2
  report_refusal
  exit 1
}

command -v jq >/dev/null 2>&1 || refuse "jq is not installed: the request cannot be read"
[[ $# -eq 1 ]] || refuse "usage: dispatch.sh <request-file>"
[[ -f "$1" && -r "$1" ]] || refuse "the request file cannot be read"
(( $(wc -c <"$1") <= 65536 )) || refuse "malformed request: larger than 64 KiB"

# NUL bytes are dropped first: the shell drops them anyway, and macOS grep
# drops a whole line that holds one.
line=$(tr -d '\000' <"$1" | grep -a -v '^[[:space:]]*$' | tail -n 1 || true)
[[ -n "$line" ]] || refuse "malformed request: the file is empty"

if ! printf '%s' "$line" | jq -e 'type == "object"' >/dev/null 2>&1; then
  line=""
  refuse "malformed request: it is not one JSON object"
fi
if ! skill=$(printf '%s' "$line" | jq -er '.skill | strings' 2>/dev/null); then
  refuse "malformed request: it names no skill"
fi
[[ "$skill" =~ ^[a-z][a-z0-9-]{0,63}$ ]] || refuse "malformed request: its skill is not a skill name"

for allowed in "${ALLOWLIST[@]}"; do
  if [[ "$skill" == "$allowed" ]]; then
    skill_to_run "$skill"
    exit 0
  fi
done
refuse "refused: the skill $skill is not on this routine's allowlist (${ALLOWLIST[*]})"
