#!/usr/bin/env bash
# Attestation ledger for /audit.
#
# Records "this (project, group) was scanned at tree hash X" so the next wave
# can skip projects whose content has not changed since the last scan. This is
# the audit analogue of the putnami task cache: judgments are cacheable outputs
# keyed by input content.
#
# State lives in .putnami/audit/attestations/ (gitignored, per-machine). One JSON
# file per (project-short, group) so parallel fleet shards never contend.
#
# Usage:
#   attest.sh hash <project-path>                  # print content hash for a project dir
#   attest.sh check <project-short> <group> <hash> # exit 0 if attested at this hash (skip scan)
#   attest.sh record <project-short> <group> <hash>  # record a completed scan
#   attest.sh list                                  # show all attestations
#   attest.sh clear [<project-short>]               # drop all, or one project's, attestations
#
# Hash semantics: `git rev-parse HEAD:<path>` — the git tree object of the
# project directory at HEAD. A dirty working tree in that path appends "-dirty"
# so the hash never matches and the project is always re-scanned. Limitation:
# the hash covers only the project's own directory, not its dependencies; a dep
# change does not invalidate dependents. Use --no-cache on /audit for a full
# forced wave.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
DIR="$ROOT/.putnami/audit/attestations"

usage() {
  sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

cmd="${1:-}"
[ -n "$cmd" ] || usage
shift

case "$cmd" in
  hash)
    path="${1:?usage: attest.sh hash <project-path>}"
    rel="${path#/}"
    rel="${rel%/}"
    tree="$(git -C "$ROOT" rev-parse "HEAD:$rel" 2>/dev/null || echo "no-tree")"
    if [ -n "$(git -C "$ROOT" status --porcelain -- "$rel")" ]; then
      echo "${tree}-dirty"
    else
      echo "$tree"
    fi
    ;;
  check)
    short="${1:?project-short}"; group="${2:?group}"; hash="${3:?hash}"
    case "$hash" in *-dirty|no-tree*) exit 1 ;; esac
    f="$DIR/${short}__${group}.json"
    [ -f "$f" ] || exit 1
    stored="$(jq -r '.treeHash' "$f")"
    [ "$stored" = "$hash" ]
    ;;
  record)
    short="${1:?project-short}"; group="${2:?group}"; hash="${3:?hash}"
    case "$hash" in
      *-dirty|no-tree*)
        echo "attest: refusing to record unstable hash '$hash' (dirty tree or untracked path)" >&2
        exit 1
        ;;
    esac
    mkdir -p "$DIR"
    jq -n \
      --arg project "$short" --arg group "$group" --arg treeHash "$hash" \
      --arg headSha "$(git -C "$ROOT" rev-parse HEAD)" \
      --arg verifiedAt "$(date +%Y-%m-%d)" \
      '{project: $project, group: $group, treeHash: $treeHash, headSha: $headSha, verifiedAt: $verifiedAt}' \
      > "$DIR/${short}__${group}.json"
    ;;
  list)
    if [ ! -d "$DIR" ] || [ -z "$(ls -A "$DIR" 2>/dev/null)" ]; then
      echo "no attestations recorded"
      exit 0
    fi
    printf '%-24s %-24s %-14s %s\n' PROJECT GROUP DATE TREE
    for f in "$DIR"/*.json; do
      jq -r '[.project, .group, .verifiedAt, .treeHash[0:12]] | @tsv' "$f"
    done | sort | while IFS=$'\t' read -r p g d t; do
      printf '%-24s %-24s %-14s %s\n' "$p" "$g" "$d" "$t"
    done
    ;;
  clear)
    short="${1:-}"
    if [ -n "$short" ]; then
      rm -f "$DIR/${short}__"*.json
    else
      rm -f "$DIR"/*.json 2>/dev/null || true
    fi
    ;;
  *)
    usage
    ;;
esac
