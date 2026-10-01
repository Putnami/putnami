#!/usr/bin/env bash
# English-only detector. One deterministic check serves three callers:
# the fix finalizer (a proposal's own title and body), /check (the branch's
# commits, changed files and proposal), and /audit's `prop/language` rule (every
# open task, and every tracked file). No dependency beyond bash, git, grep, awk,
# sort; the `tasks` mode also needs jq and a Putnami CLI whose workspace binds a
# tasks provider.
#
# Usage:
#   bash english-only.sh text [--label <name>] [<file>...]   # no file or "-": stdin
#   bash english-only.sh files [<pathspec>...]                # git-tracked files
#   bash english-only.sh files --diff <revision-range>        # files the range changes
#   bash english-only.sh commits <revision-range>             # one unit per commit message
#   bash english-only.sh tasks [--limit <n>]                  # open tasks of the tasks provider
#   bash english-only.sh github [--limit <n>]                 # deprecated alias of tasks
#
# Exit status: 0 no offender, 1 offenders listed on stdout, 2 usage or tool
# failure. A 2 is never "clean".
#
# Heuristic. Each unit (a text, a file, a commit message, an issue or PR title
# plus body) is normalized, then scored on three signal kinds:
#   - a French phrase or word from FRENCH_WORDS (whole word, case-insensitive);
#   - a distinct word containing a French accented letter from ACCENTS;
#   - a distinct common French word from FRENCH_COMMON (whole word, lowercase or
#     capitalized, so DES, EST or LA written as acronyms never count).
# A unit is an offender when it has at least one FRENCH_WORDS hit, or at least
# two distinct accented words, or at least two distinct FRENCH_COMMON words.
# One accented word alone (a UTF-8 fixture such as a quoted test string, a
# borrowed word, a name) never fires, and neither does one common word that is
# also English ("Des Moines").
# Normalization blanks fenced code blocks and inline `code` spans, where
# fixtures are quoted, drops command-line options such as `ls -la`, and removes
# the proper nouns in PROPER_NOUNS. Line numbers are preserved so the listing
# points at the source.
#
# Exclusions for the `files` mode: test files (*_test.go, *.test.*, *.spec.*,
# *_test.py, test_*.py), testdata/ and fixtures/ directories, and this detector
# itself, which necessarily spells out the signals it looks for.
# The `tasks` mode skips the single audit tracking task (fingerprint line
# `audit-fp: workspace/language/english-only`), which quotes the offenders. It
# reads tasks in the open, in_progress and blocked states through
# `putnami tasks find`, page by page; change proposals are not enumerable through
# the proposals contract (it finds them by base and head), so no mode lists them.
set -euo pipefail

# Byte-wise matching keeps the heuristic identical under GNU and BSD grep and
# independent of the caller's locale.
export LC_ALL=C

# Under Git for Windows a native jq.exe ends every output line with CRLF, and
# the CR left in a captured value fails every comparison; --binary keeps LF.
case "${OSTYPE:-}" in
  msys* | cygwin*) jq() { command jq --binary "$@"; } ;;
esac

ACCENTS='é|è|ê|à|ç|ù|œ|É|È|Ê|À|Ç|Ù|Œ'
# French words and phrases that are never English: one is enough.
FRENCH_WORDS="nous|avec|pour que|il faut|ainsi|donc|c'est|parce que|il y a"
# Common French words that carry short sentences such as a commit subject
# ("corrige le parseur des tableaux"). A few are also English ("Des Moines"),
# so two distinct ones are needed, matched lowercase or capitalized only.
FRENCH_COMMON='le la les des une du aux est sont dans sur pas mais aussi cette ces qui que ajoute supprime corrige fichier fichiers'
# Proper nouns kept in their original language, one per line: the French
# data-protection regulation and authority. A name may wrap across lines.
PROPER_NOUNS="RGPD
CNIL
Règlement Général sur la Protection des Données
Commission Nationale de l'Informatique et des Libertés"
TRACKER_MARKER='audit-fp: workspace/language/english-only'
MAX_LINES=5
MAX_SIGNALS=10

usage() {
  cat <<'USAGE'
Usage:
  bash english-only.sh text [--label <name>] [<file>...]   # no file or "-": stdin
  bash english-only.sh files [<pathspec>...]                # git-tracked files
  bash english-only.sh files --diff <revision-range>        # files the range changes
  bash english-only.sh commits <revision-range>             # one unit per commit message
  bash english-only.sh tasks [--limit <n>]                  # open tasks of the tasks provider
  bash english-only.sh github [--limit <n>]                 # deprecated alias of tasks

Exit status: 0 no offender, 1 offenders listed on stdout, 2 usage or tool failure.
USAGE
}

die() {
  echo "english-only: $*" >&2
  exit 2
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
OFFENDERS=0
UNITS=0

# normalize <file>: blank fenced blocks, inline code spans, command-line
# options such as `ls -la`, and proper nouns, keeping every line in place.
normalize() {
  # The list reaches awk through the environment: awk -v rejects a newline.
  ENGLISH_ONLY_NOUNS="$PROPER_NOUNS" awk '
    /^[[:space:]]*(```|~~~)/ { fence = !fence; text = text "\n"; next }
    fence { text = text "\n"; next }
    {
      gsub(/`[^`]*`/, "")
      gsub(/(^|[[:space:]])--?[[:alnum:]][[:alnum:]-]*/, " ")
      text = text $0 "\n"
    }
    END {
      n = split(ENVIRON["ENGLISH_ONLY_NOUNS"], noun, "\n")
      for (i = 1; i <= n; i++) {
        re = noun[i]
        gsub(/ /, "[[:space:]]+", re)
        gsub(/\047/, "(\047|’)", re)
        # Blank the name but keep its line breaks, so line numbers still hold.
        while (match(text, re)) {
          seg = substr(text, RSTART, RLENGTH)
          gsub(/[^\n]/, " ", seg)
          text = substr(text, 1, RSTART - 1) seg substr(text, RSTART + RLENGTH)
        }
      }
      printf "%s", text
    }
  ' "$1"
}

# signals <file>: one "<kind> <signal><TAB><line>" record per hit, where kind
# 1 is a FRENCH_WORDS hit, 2 a FRENCH_COMMON word, 3 an accented word. Words
# are split on spaces and ASCII punctuation, which awk and every grep agree on.
signals() {
  awk -v strong="$FRENCH_WORDS" -v common="$FRENCH_COMMON" -v accents="$ACCENTS" '
    BEGIN {
      n = split(strong, list, "|")
      for (i = 1; i <= n; i++) { w = list[i]; gsub(/\047/, " ", w); STRONG[w] = 1 }
      n = split(common, list, " ")
      for (i = 1; i <= n; i++) {
        w = list[i]
        COMMON[w] = 1
        COMMON[toupper(substr(w, 1, 1)) substr(w, 2)] = 1
      }
    }
    {
      line = $0
      gsub(/[[:space:][:punct:]]+/, " ", line)
      n = split(line, t, " ")
      for (i = 1; i <= n; i++) {
        w = tolower(t[i])
        if (w in STRONG) print "1 " w "\t" NR
        if (i < n) {
          w2 = w " " tolower(t[i + 1])
          if (w2 in STRONG) print "1 " w2 "\t" NR
          if (i + 1 < n && (w2 " " tolower(t[i + 2])) in STRONG) print "1 " w2 " " tolower(t[i + 2]) "\t" NR
        }
        if (t[i] in COMMON) print "2 " w "\t" NR
        if (t[i] ~ accents) print "3 " t[i] "\t" NR
      }
    }
  ' "$1"
}

# distinct <kind>: the number of distinct signals of one kind in $WORK/hits.
distinct() {
  { grep "^$1 " "$WORK/hits" || true; } | cut -f1 | sort -u | grep -c . || true
}

# scan_unit <label> <file>: score one unit; print it and count it when it fires.
scan_unit() {
  local label="$1" file="$2" norm="$WORK/norm"
  UNITS=$((UNITS + 1))
  normalize "$file" >"$norm"
  signals "$norm" >"$WORK/hits"
  if [ "$(distinct 1)" -eq 0 ] && [ "$(distinct 3)" -lt 2 ] && [ "$(distinct 2)" -lt 2 ]; then
    return 0
  fi
  # A lone common word is English as often as French: keep it out of the listing.
  if [ "$(distinct 2)" -lt 2 ]; then
    { grep -v "^2 " "$WORK/hits" || true; } >"$WORK/kept"
    mv "$WORK/kept" "$WORK/hits"
  fi
  OFFENDERS=$((OFFENDERS + 1))
  printf '%s: non-English signals: %s\n' "$label" "$(cut -f1 "$WORK/hits" | sort -u | cut -c3- |
    awk -v max="$MAX_SIGNALS" 'NR <= max { out = out (NR > 1 ? ", " : "") $0 }
      END { if (NR > max) out = out sprintf(" (+%d more)", NR - max); print out }')"
  cut -f2 "$WORK/hits" | sort -n -u | awk -v max="$MAX_LINES" 'NR <= max' >"$WORK/lines"
  awk 'NR == FNR { want[$1] = 1; next } FNR in want { print FNR ":" $0 }' "$WORK/lines" "$norm" |
    cut -c1-200 | while IFS= read -r line; do printf '%s:%s\n' "$label" "$line"; done
}

mode_text() {
  local label="" file
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --label) label="${2:?--label needs a value}"; shift ;;
      --) shift; break ;;
      -?*) die "text: unknown argument '$1'" ;;
      *) break ;;
    esac
    shift
  done
  if [ "$#" -eq 0 ]; then
    set -- -
  fi
  for file in "$@"; do
    if [ "$file" = "-" ]; then
      cat >"$WORK/stdin"
      scan_unit "${label:-stdin}" "$WORK/stdin"
    else
      [ -f "$file" ] || die "text: no such file: $file"
      scan_unit "${label:-$file}" "$file"
    fi
  done
}

excluded_path() {
  case "$1" in
    *_test.go | *.test.* | *.spec.* | *_test.py | test_*.py | */test_*.py) return 0 ;;
    testdata/* | */testdata/* | fixtures/* | */fixtures/*) return 0 ;;
    english-only.sh | */english-only.sh) return 0 ;;
  esac
  return 1
}

# git_grep_list <grep-args...>: tracked files matching, as paths from the
# repository root, tolerating "no match".
git_grep_list() {
  local status=0
  git -c core.quotePath=false grep -l -I --full-name "$@" || status=$?
  [ "$status" -le 1 ] || die "files: git grep failed ($status)"
}

# Pathspecs resolve from the caller's directory, as in git; with none, the
# whole repository is scanned. --diff scans the files a revision range adds or
# modifies, and nothing when it changes none.
mode_files() {
  local root path range="" status=0
  root="$(git rev-parse --show-toplevel)" || die "files: not inside a git work tree"
  if [ "${1:-}" = "--diff" ]; then
    range="${2:?--diff needs a revision range}"
    shift 2
    [ "$#" -eq 0 ] || die "files: --diff takes no pathspec"
    git -c core.quotePath=false diff --no-relative --name-only --diff-filter=d "$range" >"$WORK/changed" ||
      die "files: invalid revision range: $range"
    [ -s "$WORK/changed" ] || return 0
    while IFS= read -r path; do
      set -- "$@" ":(top,literal)$path"
    done <"$WORK/changed"
  fi
  [ "${1:-}" != "--" ] || shift
  [ "$#" -gt 0 ] || set -- ":/"
  {
    git_grep_list -E "$ACCENTS" -- "$@"
    git_grep_list -i -w -E "$FRENCH_WORDS" -- "$@"
    git_grep_list -i -w -E "$(printf '%s' "$FRENCH_COMMON" | tr ' ' '|')" -- "$@"
  } | sort -u >"$WORK/candidates" || status=$?
  [ "$status" -eq 0 ] || exit "$status"
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    excluded_path "$path" && continue
    [ -f "$root/$path" ] || continue
    scan_unit "$path" "$root/$path"
  done <"$WORK/candidates"
}

mode_commits() {
  local range="${1:-}" sha
  [ -n "$range" ] || die "commits: a revision range is required (e.g. main..HEAD)"
  git rev-list "$range" >"$WORK/commits" || die "commits: invalid revision range: $range"
  while IFS= read -r sha; do
    git log -1 --format=%B "$sha" >"$WORK/commit"
    scan_unit "commit $(git rev-parse --short "$sha")" "$WORK/commit"
  done <"$WORK/commits"
}

# workspace_cli prints the CLI of the workspace the caller is in: its executable
# wrapper when it has one, otherwise the installed CLI.
workspace_cli() {
  if [ -x "$1/putnamiw" ]; then
    printf '%s\n' "$1/putnamiw"
  elif command -v putnami >/dev/null 2>&1; then
    printf '%s\n' putnami
  else
    return 1
  fi
}

# mode_tasks: scan the title and body of every open, in-progress or blocked task
# the workspace's tasks provider returns, following page.next until the last
# page or --limit tasks.
mode_tasks() {
  local limit=1000 root cli cursor="" request envelope outcome count=0 index total label
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --limit) limit="${2:?--limit needs a value}"; shift ;;
      --repo) die "tasks: --repo is not supported: the workspace's tasks binding names the repository" ;;
      *) die "tasks: unknown argument '$1'" ;;
    esac
    shift
  done
  case "$limit" in
    '' | *[!0-9]* | 0) die "tasks: --limit must be a positive integer" ;;
  esac
  command -v jq >/dev/null || die "tasks: jq not found"
  root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
  cli="$(workspace_cli "$root")" || die "tasks: no Putnami CLI found — expected ./putnamiw or putnami on PATH"
  while :; do
    request="$(jq -cn --arg cursor "$cursor" \
      '{states: ["open", "in_progress", "blocked"], page: ({size: 100} + (if $cursor == "" then {} else {cursor: $cursor} end))}')"
    envelope="$(cd "$root" && "$cli" tasks find --input "$request" --output=json)" || true
    outcome="$(jq -r '.outcome // empty' <<<"$envelope" 2>/dev/null || true)"
    [ "$outcome" = ok ] ||
      die "tasks: tasks find answered ${outcome:-no envelope}: $(jq -r '.error.message // empty' <<<"$envelope" 2>/dev/null || true)"
    printf '%s\n' "$envelope" >"$WORK/page.json"
    total="$(jq '.result.items | length' "$WORK/page.json")"
    index=0
    while [ "$index" -lt "$total" ] && [ "$count" -lt "$limit" ]; do
      jq -r --argjson i "$index" '.result.items[$i] | .title + "\n\n" + (.body // "")' "$WORK/page.json" >"$WORK/item"
      label="task $(jq -r --argjson i "$index" '.result.items[$i].ref.id' "$WORK/page.json")"
      index=$((index + 1))
      count=$((count + 1))
      if grep -Fq "$TRACKER_MARKER" "$WORK/item"; then
        continue
      fi
      scan_unit "$label" "$WORK/item"
    done
    [ "$count" -lt "$limit" ] || break
    cursor="$(jq -r '.result.page.next // ""' "$WORK/page.json")"
    [ -n "$cursor" ] || break
  done
}

MODE="${1:-}"
[ "$#" -eq 0 ] || shift
case "$MODE" in
  text) mode_text "$@" ;;
  files) mode_files "$@" ;;
  commits) mode_commits "$@" ;;
  tasks) mode_tasks "$@" ;;
  github)
    echo "english-only: the github mode is a deprecated alias of the tasks mode: it scans open tasks through the workspace's tasks binding; change proposals are not scanned" >&2
    mode_tasks "$@"
    ;;
  -h | --help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

if [ "$OFFENDERS" -gt 0 ]; then
  echo "english-only: $OFFENDERS of $UNITS scanned unit(s) are not in English" >&2
  exit 1
fi
echo "english-only: $UNITS scanned unit(s), no offender" >&2
