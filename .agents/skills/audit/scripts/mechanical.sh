#!/usr/bin/env bash
# Deterministic mechanical checks for /audit.
#
# These properties are lint rules, not judgment calls — running them through an
# LLM is nondeterministic and expensive. /audit runs this script once per
# project and files issues from its output; the model's judgment is reserved
# for triaging results (e.g. a CLI output writer legitimately printing to
# stdout) and for the semantic properties.
#
# Usage: mechanical.sh <project-path>        # e.g. mechanical.sh surfaces/workloads/api
#
# Emits one JSON line per finding on stdout:
#   {"property": "...", "file": "...", "lines": [...], "value": N, "threshold": N, "message": "..."}
#
# Checks:
#   complexity-file-size      — Go files > 500 lines, TS files > 400 lines
#   complexity-nesting        — max indent depth >= 6 units (tabs, or 2-space)
#   complexity-dependencies   — fan-out > 8 imports of the workspace's own modules
#                               (config.sh modules) or relative TS imports, per file
#   ops-logging               — console.* (TS) / fmt.Print* & println (Go)
#
# When the workspace sets options["@putnami/intelligence"].audit.checks, that
# repository script runs last with the same project path and its JSON lines join
# this output; its exit status is this script's. It owns the checks that only
# make sense in that repository.
#
# Test files, generated code (.gen/, *.d.ts), node_modules, dist, vendor, and
# testdata are excluded. Only git-tracked files are scanned.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(git rev-parse --show-toplevel)"
path="${1:?usage: mechanical.sh <project-path>}"
rel="${path#/}"
rel="${rel%/}"
cd "$ROOT"
[ -d "$rel" ] || { echo "mechanical: no such directory: $rel" >&2; exit 2; }

emit() { # property file value threshold message lines-json
  jq -nc --arg property "$1" --arg file "$2" --argjson value "$3" \
         --argjson threshold "$4" --arg message "$5" --argjson lines "$6" \
         '{property: $property, file: $file, lines: $lines, value: $value, threshold: $threshold, message: $message}'
}

lines_json() { # newline-separated line numbers on stdin -> JSON array (first 10)
  head -10 | jq -cnR '[inputs | tonumber]'
}

# repository_checks runs the workspace's own checks script, when one is set,
# and exits with its status.
repository_checks() {
  local checks status=0
  checks="$(bash "$HERE/config.sh" checks)" || status=$?
  case "$status" in
    0) bash "$ROOT/$checks" "$rel"; exit $? ;;
    1) exit 0 ;;
    *) exit 2 ;;
  esac
}

files="$(git ls-files -- "$rel" \
  | grep -E '\.(go|ts|tsx)$' \
  | grep -vE '(^|/)(node_modules|dist|testdata|vendor|\.gen)/' \
  | grep -vE '(_test\.go|\.test\.tsx?|\.spec\.tsx?|\.d\.ts)$' || true)"

[ -n "$files" ] || { echo "mechanical: no source files in $rel" >&2; repository_checks; }

# Fan-out counts imports of the workspace's own modules (config.sh modules).
modules="$(bash "$HERE/config.sh" modules)" || exit 2
go_mods="$(printf '%s\n' "$modules" | grep -v '^@' | sed 's/\./\\./g' | paste -sd '|' - || true)"
ts_mods="$(printf '%s\n' "$modules" | grep '^@' | sed -e 's/\./\\./g' -e 's|$|/|' | paste -sd '|' - || true)"

count_files=0
count_findings=0

while IFS= read -r f; do
  count_files=$((count_files + 1))
  case "$f" in
    *.go) lang=go; size_threshold=500 ;;
    *)    lang=ts; size_threshold=400 ;;
  esac

  # complexity-file-size
  nlines="$(wc -l < "$f" | tr -d ' ')"
  if [ "$nlines" -gt "$size_threshold" ]; then
    emit complexity-file-size "$f" "$nlines" "$size_threshold" \
      "file has $nlines lines (threshold $size_threshold for $lang)" '[]'
    count_findings=$((count_findings + 1))
  fi

  # complexity-nesting — max indent depth in units (tabs, or 2 spaces = 1 unit)
  read -r maxdepth maxline <<EOF
$(awk '
  /^[ \t]*$/ { next }
  {
    n = 0
    while (substr($0, n + 1, 1) == "\t") n++
    if (n == 0) {
      s = 0
      while (substr($0, s + 1, 1) == " ") s++
      n = int(s / 2)
    }
    if (n > max) { max = n; maxline = NR }
  }
  END { printf "%d %d\n", max, maxline }
' "$f")
EOF
  if [ "$maxdepth" -ge 6 ]; then
    emit complexity-nesting "$f" "$maxdepth" 6 \
      "max indent depth $maxdepth units at line $maxline (nesting > 4 levels)" "[$maxline]"
    count_findings=$((count_findings + 1))
  fi

  # complexity-dependencies — internal import fan-out
  if [ "$lang" = go ]; then
    fanout=0
    [ -z "$go_mods" ] || fanout="$(grep -cE "^[[:space:]]*([[:alnum:]_]+ )?\"($go_mods)/" "$f" || true)"
  else
    fanout="$(grep -cE "from ['\"](\.${ts_mods:+|$ts_mods})" "$f" || true)"
  fi
  if [ "$fanout" -gt 8 ]; then
    emit complexity-dependencies "$f" "$fanout" 8 \
      "$fanout internal imports (fan-out threshold 8)" '[]'
    count_findings=$((count_findings + 1))
  fi

  # ops-logging — raw prints instead of the structured logger
  if [ "$lang" = go ]; then
    pattern='\bfmt\.Print(ln|f)?\(|^[[:space:]]*println\('
  else
    pattern='\bconsole\.(log|warn|error|info|debug|trace)\('
  fi
  hits="$(grep -nE "$pattern" "$f" | cut -d: -f1 || true)"
  if [ -n "$hits" ]; then
    nhits="$(printf '%s\n' "$hits" | wc -l | tr -d ' ')"
    emit ops-logging "$f" "$nhits" 0 \
      "$nhits raw print/console call(s) — code must use the structured logger" \
      "$(printf '%s\n' "$hits" | lines_json)"
    count_findings=$((count_findings + 1))
  fi
done <<EOF
$files
EOF

echo "mechanical: scanned $count_files files in $rel, $count_findings findings" >&2

repository_checks
