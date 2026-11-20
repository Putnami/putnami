#!/usr/bin/env bash
# English-only scan of change proposals, for /audit's `prop/language` rule.
#
# The proposals contract finds a proposal by its exact base and head; it has no
# "list every open proposal" operation. The heads therefore come from Git: the
# remote-tracking branches of <remote> except the base, most recently committed
# first. For each one this asks the workspace's proposals binding (`putnami
# proposals find`, one provider call per head and page) for its open and draft
# proposals into <base>, and scans each proposal's title and body with the
# shared detector's `text` mode. No hosting service is called directly.
#
# Usage:
#   english-only-proposals.sh [--remote <name>] [--base <branch>] [--limit <n>] [--heads <n>]
#
# --remote defaults to origin. --base defaults to the branch <remote>/HEAD names,
# then to main. --limit stops after that many proposals (default 1000). --heads
# asks about that many heads at most (default 200); the summary counts the
# heads left out. Fetch the remote first: the heads are the local
# remote-tracking refs.
#
# A head whose find answers `unavailable` is skipped with a warning and counted
# as unscanned; any other failure stops the scan.
#
# Exit status: 0 no offender and no unavailable head (the summary counts the
# heads beyond --heads), 1 offenders listed on stdout, 2 usage or tool failure,
# or no offender while a head was unavailable. A 2 is never "clean".
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DETECTOR="$SCRIPT_DIR/../../check/scripts/english-only.sh"

die() {
  echo "english-only-proposals: $*" >&2
  exit 2
}

remote=origin
base=""
limit=1000
max_heads=200
while [ "$#" -gt 0 ]; do
  case "$1" in
    --remote) remote="${2:?--remote needs a value}"; shift ;;
    --base) base="${2:?--base needs a value}"; shift ;;
    --limit) limit="${2:?--limit needs a value}"; shift ;;
    --heads) max_heads="${2:?--heads needs a value}"; shift ;;
    -h | --help) sed -n '2,26p' "$0"; exit 0 ;;
    *) die "unknown argument '$1'" ;;
  esac
  shift
done
case "$limit" in
  '' | *[!0-9]* | 0) die "--limit must be a positive integer" ;;
esac
case "$max_heads" in
  '' | *[!0-9]* | 0) die "--heads must be a positive integer" ;;
esac
[ -f "$DETECTOR" ] || die "the shared detector is missing: $DETECTOR"
command -v jq >/dev/null || die "jq not found"
# Under Git for Windows a native jq.exe ends every output line with CRLF, and
# the CR left in a captured value fails every comparison; --binary keeps LF.
case "${OSTYPE:-}" in
  msys* | cygwin*) jq() { command jq --binary "$@"; } ;;
esac

root="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not inside a git work tree"
if [ -x "$root/putnamiw" ]; then
  cli="$root/putnamiw"
elif command -v putnami >/dev/null 2>&1; then
  cli=putnami
else
  die "no Putnami CLI found — expected ./putnamiw or putnami on PATH"
fi
if [ -z "$base" ]; then
  base="$(git -C "$root" symbolic-ref --quiet --short "refs/remotes/$remote/HEAD" 2>/dev/null || true)"
  base="${base#"$remote"/}"
  base="${base:-main}"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

git -C "$root" for-each-ref --sort=-committerdate --format='%(refname:strip=3)' "refs/remotes/$remote/" >"$WORK/refs" ||
  die "cannot list the branches of remote $remote"
while IFS= read -r head; do
  if [ -n "$head" ] && [ "$head" != HEAD ] && [ "$head" != "$base" ]; then
    printf '%s\n' "$head"
  fi
done <"$WORK/refs" >"$WORK/heads"
candidates="$(wc -l <"$WORK/heads" | tr -d '[:space:]')"

heads=0
unavailable=0
scanned=0
offenders=0
while IFS= read -r head; do
  [ "$scanned" -lt "$limit" ] || break
  [ "$heads" -lt "$max_heads" ] || break
  heads=$((heads + 1))
  cursor=""
  while :; do
    request="$(jq -cn --arg base "$base" --arg head "$head" --arg cursor "$cursor" \
      '{change: {base: $base, head: $head}, states: ["draft", "open"], page: ({size: 100} + (if $cursor == "" then {} else {cursor: $cursor} end))}')"
    envelope="$(cd "$root" && "$cli" proposals find --input "$request" --output=json)" || true
    outcome="$(jq -r '.outcome // empty' <<<"$envelope" 2>/dev/null || true)"
    if [ "$outcome" = unavailable ]; then
      echo "english-only-proposals: skipping $head, unscanned: proposals find ($base <- $head) answered unavailable: $(jq -r '.error.message // empty' <<<"$envelope" 2>/dev/null || true)" >&2
      unavailable=$((unavailable + 1))
      break
    fi
    [ "$outcome" = ok ] ||
      die "proposals find ($base <- $head) answered ${outcome:-no envelope}: $(jq -r '.error.message // empty' <<<"$envelope" 2>/dev/null || true)"
    printf '%s\n' "$envelope" >"$WORK/page.json"
    total="$(jq '.result.items | length' "$WORK/page.json")"
    index=0
    while [ "$index" -lt "$total" ] && [ "$scanned" -lt "$limit" ]; do
      jq -r --argjson i "$index" '.result.items[$i] | .title + "\n\n" + (.body // "")' "$WORK/page.json" >"$WORK/item"
      label="proposal $(jq -r --argjson i "$index" '.result.items[$i].ref.id' "$WORK/page.json")"
      index=$((index + 1))
      scanned=$((scanned + 1))
      status=0
      bash "$DETECTOR" text --label "$label" "$WORK/item" 2>"$WORK/detector.err" || status=$?
      case "$status" in
        0) ;;
        1) offenders=$((offenders + 1)) ;;
        *) cat "$WORK/detector.err" >&2; die "the detector failed on $label" ;;
      esac
    done
    [ "$scanned" -lt "$limit" ] || break
    cursor="$(jq -r '.result.page.next // ""' "$WORK/page.json")"
    [ -n "$cursor" ] || break
  done
done <"$WORK/heads"

# Heads beyond --heads are counted only when --limit did not stop the scan
# first: the proposal bound is the one the caller asked to reach.
left_out=0
if [ "$scanned" -lt "$limit" ]; then
  left_out=$((candidates - heads))
fi
coverage="$heads of $candidates head(s) of $remote asked, $unavailable unavailable, $left_out beyond --heads $max_heads"
if [ "$offenders" -gt 0 ]; then
  echo "english-only-proposals: $offenders of $scanned proposal(s) into $base are not in English ($coverage)" >&2
  exit 1
fi
if [ "$unavailable" -gt 0 ]; then
  echo "english-only-proposals: $scanned proposal(s) into $base scanned, no offender, but the scan is incomplete ($coverage)" >&2
  exit 2
fi
echo "english-only-proposals: $scanned proposal(s) into $base scanned, no offender ($coverage)" >&2
