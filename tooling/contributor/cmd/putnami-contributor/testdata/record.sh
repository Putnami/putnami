#!/usr/bin/env bash
# Records the parity goldens from the core command. The test does not run it:
# it compares the extension executable with the files this script wrote, so
# the goldens stay the core's answer.
#
# Usage: record.sh <putnami-binary> <out-directory>
#
# <putnami-binary> must serve `putnami contributor language`. The current core
# refuses that root, so the binary is a putnami built from a commit before the
# core deleted it. The script stops when the binary refuses the command.
#
# The script builds the fixture repository with fixture.sh, adds a minimal
# workspace file because the core command needs one, runs
# `putnami contributor language <arguments>` for every line of
# parity/cases.txt, and writes parity/<name>.golden into <out-directory>.
set -euo pipefail

putnami="$(cd "$(dirname "${1:?usage: record.sh <putnami-binary> <out-directory>}")" && pwd)/$(basename "$1")"
here="$(cd "$(dirname "$0")" && pwd)"
out="$(mkdir -p "${2:?usage: record.sh <putnami-binary> <out-directory>}" && cd "$2" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

"$here/fixture.sh" "$work/repo"
printf '{"name":"fixture"}\n' > "$work/repo/putnami.workspace.json"
cd "$work/repo"

while IFS='|' read -r name input arguments; do
  case "$name" in '' | '#'*) continue ;; esac
  stdin_file=/dev/null
  if [ -n "$input" ]; then
    printf '%s\n' "$input" > "$work/stdin"
    stdin_file="$work/stdin"
  fi
  code=0
  # shellcheck disable=SC2086
  "$putnami" contributor language $arguments < "$stdin_file" > "$work/stdout" 2> "$work/stderr" || code=$?
  if [ "$code" = 2 ] && grep -q 'is no longer a core command' "$work/stderr"; then
    echo "record.sh: $putnami refuses contributor language; use a putnami built before the core deleted it" >&2
    exit 1
  fi
  {
    printf 'exit %s\n-- stdout --\n' "$code"
    cat "$work/stdout"
    printf -- '-- stderr --\n'
    cat "$work/stderr"
  } > "$out/$name.golden"
done < "$here/parity/cases.txt"
