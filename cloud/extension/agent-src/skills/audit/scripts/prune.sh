#!/usr/bin/env bash
# Deterministic candidate finder for `/audit --prune`.
#
# Prune mode removes weight instead of filing quality findings. This script
# emits *candidates*; the model triages them and the owner gives verdicts on
# the ones that need a decision. Everything here is grep-level and cheap, so it
# runs once per scope in seconds and never burns model context on counting.
#
# Consumers live in this repository and in the repositories that depend on
# it. A symbol or package with no importer in any of them is dead. Set
# PUTNAMI_CONSUMER_REPOS to a colon-separated list of their checkouts; set it
# to an empty string when this repository has no consumer. `index` and `repos`
# refuse to run while it is unset.
#
# Usage:
#   prune.sh index                       # (re)build the symbol and import index over every repository
#   prune.sh scan <typology> <path>...   # emit JSONL candidates for one typology under the given paths
#   prune.sh scan all <path>...          # every typology
#   prune.sh repos                       # print the consumer repositories and their HEAD age
#
# Typologies:
#   dead            packages with no importer outside themselves, exported
#                   symbols referenced nowhere outside their own package, and
#                   exports referenced only from tests
#   duplicate       the same free-function name defined in two or more
#                   projects (copy-paste between siblings)
#   multi-version   identifiers and files carrying legacy / v1 / compat /
#                   fallback vocabulary, and the .putnamirc fallback
#   palliative      guards whose comment cites an incident, and tests pinning
#                   repository state (commit SHAs, stamped versions)
#   historical-ref  comments and docs citing an issue, PR or commit
#   comment         justifying comments, lint escapes (//nolint, as any,
#                   @ts-ignore, biome-ignore), files with > 30 % comment lines
#   test-scaffold   test-only packages, helper/fake/mock files, projects with
#                   test lines > 2x source lines
#   config-surface  PUTNAMI_* environment variables read in code but absent
#                   from every doc/ page
#   doc             ADRs marked superseded/rejected/proposed, doc pages that
#                   link to a path which no longer exists
#
# Output: one JSON line per candidate:
#   {"typology": "...", "file": "...", "lines": [...], "subject": "...", "value": N,
#    "auto": true|false, "message": "..."}
# `auto` is true when the removal needs no owner verdict (the evidence is
# complete: zero consumers, a dangling reference, a comment). Everything else
# goes to the umbrella issue's verdict table.
#
# The workspace's own packages are the Go module paths of its tracked go.mod
# files and the scoped npm names of its tracked package.json files; imports are
# counted against those prefixes, so the script carries no repository name.
#
# Generated code (.gen/, *.d.ts, dist/, files whose first line carries a
# "Code generated ... DO NOT EDIT" or "Auto-generated" marker), node_modules,
# vendor and testdata are excluded. Only git-tracked files are scanned.
# `dead` skips exports named in their own module's AI.md, README.md or doc/
# pages (ADRs excluded): those are published entry points, and shrinking them
# is a doc decision.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SELF="$HERE/$(basename "${BASH_SOURCE[0]}")"
ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"
IDX="$ROOT/.putnami/audit/prune"
mkdir -p "$IDX"

REPOS="${PUTNAMI_CONSUMER_REPOS-}"
require_repos() {
  if [ -z "${PUTNAMI_CONSUMER_REPOS+set}" ]; then
    echo "prune: set PUTNAMI_CONSUMER_REPOS to the colon-separated consumer checkouts (empty when there is none)" >&2
    exit 2
  fi
}

usage() { sed -n '2,57p' "$SELF" | sed 's/^# \{0,1\}//'; exit 2; }

emit() { # typology file lines-json subject value auto message
  jq -nc --arg typology "$1" --arg file "$2" --argjson lines "$3" --arg subject "$4" \
         --argjson value "$5" --argjson auto "$6" --arg message "$7" \
         '{typology: $typology, file: $file, lines: $lines, subject: $subject, value: $value, auto: $auto, message: $message}'
}
lines_json() { head -10 | jq -cnR '[inputs | tonumber]'; }

# Drop files whose first line is a generator marker. Reads paths on stdin.
drop_generated() {
  local files gen
  files="$(cat)"; [ -n "$files" ] || return 0
  gen="$(printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 -r awk 'FNR == 1 { if ($0 ~ /Code generated .* DO NOT EDIT|^\/\/ Auto-generated/) print FILENAME; nextfile }' 2>/dev/null || true)"
  if [ -n "$gen" ]; then printf '%s\n' "$files" | grep -vxF -f <(printf '%s\n' "$gen") || true
  else printf '%s\n' "$files"; fi
}

# Source files under the given paths, tracked, non-generated. $1 = "src" | "test" | "all"
tracked() { # kind path...
  local kind="$1"; shift
  git ls-files -- "$@" \
    | grep -E '\.(go|ts|tsx)$' \
    | grep -vE '(^|/)(node_modules|vendor|dist|testdata|\.gen)/|\.d\.ts$' \
    | case "$kind" in
        src)  grep -vE '(_test\.go|\.test\.tsx?|\.spec\.tsx?)$' ;;
        test) grep -E '(_test\.go|\.test\.tsx?|\.spec\.tsx?)$' ;;
        all)  cat ;;
      esac \
    | drop_generated || true
}
RG_COMMON=(-g '!node_modules' -g '!vendor' -g '!dist' -g '!testdata' -g '!.gen' -g '!*.d.ts' -g '!*.sum' -g '!*.lock')

# ---------------------------------------------------------------------------
# index — one pass over this repository and its consumers
# ---------------------------------------------------------------------------
cmd_repos() {
  require_repos
  echo "workspace $ROOT  $(git log -1 --format='%h %cs')"
  local IFS=:
  for r in $REPOS; do
    if [ -d "$r/.git" ] || [ -f "$r/.git" ]; then
      echo "consumer $r  $(git -C "$r" log -1 --format='%h %cs')"
    else
      echo "consumer $r  MISSING (set PUTNAMI_CONSUMER_REPOS)" >&2
    fi
  done
}

# Module prefixes the workspace publishes (config.sh modules): Go module
# prefixes and npm scopes, one per line.
go_hosts() { bash "$HERE/config.sh" modules | grep -v '^@' || true; }
package_scopes() { bash "$HERE/config.sh" modules | grep '^@' || true; }
# import_pattern prints the ripgrep pattern of an import of a workspace
# package, or nothing when the workspace publishes no module.
import_pattern() {
  local alts=() h
  while read -r h; do [ -n "$h" ] && alts+=("$(printf '%s' "$h" | sed 's/\./\\./g')/[A-Za-z0-9/_.~-]+"); done < <(go_hosts)
  while read -r h; do [ -n "$h" ] && alts+=("$(printf '%s' "$h" | sed 's/\./\\./g')/[a-z0-9._~-]+"); done < <(package_scopes)
  [ "${#alts[@]}" -gt 0 ] || return 0
  local IFS='|'; printf '(%s)' "${alts[*]}"
}

cmd_index() {
  require_repos
  # A step that finds nothing is a result: grep and rg exit 1 on no match.
  # indexed-at is written last, so an index that stopped early is refused.
  set +o pipefail
  rm -f "$IDX/indexed-at"
  local repos=("$ROOT"); local IFS=:
  for r in $REPOS; do [ -d "$r" ] && repos+=("$r"); done
  unset IFS
  echo "index: repositories: ${repos[*]}" >&2

  # 1. Exported definitions in this repository (Go exported, TS exported).
  #    defs.tsv: symbol <TAB> file <TAB> package-dir
  {
    tracked src . \
      | grep -E '\.go$' | tr '\n' '\0' \
      | xargs -0 -r rg -H -N -o --no-heading \
          '^(func(\s*\([^)]*\))?\s+|type\s+|var\s+|const\s+)([A-Z][A-Za-z0-9_]*)' -r '$3' 2>/dev/null \
      | awk -F: '{print $2 "\t" $1 "\t" ($1 ~ /\// ? substr($1, 1, match($1, /\/[^\/]*$/)-1) : ".")}'
    tracked src . \
      | grep -E '\.tsx?$' | tr '\n' '\0' \
      | xargs -0 -r rg -H -N -o --no-heading \
          '^export\s+(async\s+)?(function|const|class|interface|type|enum|let)\s+([A-Za-z_][A-Za-z0-9_]*)' -r '$3' 2>/dev/null \
      | awk -F: '{print $2 "\t" $1 "\t" ($1 ~ /\// ? substr($1, 1, match($1, /\/[^\/]*$/)-1) : ".")}'
  } | sort -u > "$IDX/defs.tsv"
  echo "index: $(wc -l < "$IDX/defs.tsv" | tr -d ' ') exported definitions" >&2

  # 2. Every identifier occurrence in every repository, filtered to the
  #    defined names, aggregated per (symbol, file). refs.tsv: symbol file count is_test
  cut -f1 "$IDX/defs.tsv" | sort -u > "$IDX/names.txt"
  for r in "${repos[@]}"; do
    (cd "$r" && rg -H -N -o --no-heading "${RG_COMMON[@]}" -g '*.go' -g '*.ts' -g '*.tsx' \
        '\b[A-Za-z_][A-Za-z0-9_]{2,}\b' . 2>/dev/null \
      | awk -v repo="$r" -v names="$IDX/names.txt" '
          BEGIN { while ((getline n < names) > 0) want[n]=1 }
          { i = index($0, ":"); f = substr($0, 1, i-1); s = substr($0, i+1)
            if (s in want) { sub(/^\.\//, "", f); c[s "\t" repo "/" f]++ } }
          END { for (k in c) print k "\t" c[k] }')
  done | awk -F'\t' '{ t = ($2 ~ /(_test\.go|\.test\.tsx?|\.spec\.tsx?)$/) ? 1 : 0; print $0 "\t" t }' \
    > "$IDX/refs.tsv"
  echo "index: $(wc -l < "$IDX/refs.tsv" | tr -d ' ') (symbol, file) reference rows" >&2

  # 3. Package imports across every repository. imports.tsv: package file
  local pattern; pattern="$(import_pattern)"
  [ -n "$pattern" ] || pattern='$^'
  for r in "${repos[@]}"; do
    (cd "$r" && rg -H -N -o --no-heading "${RG_COMMON[@]}" -g '*.go' -g '*.ts' -g '*.tsx' -g '*.json' \
        "$pattern" . 2>/dev/null \
      | awk -v repo="$r" '{ i = index($0, ":"); f = substr($0, 1, i-1); sub(/^\.\//, "", f); print substr($0, i+1) "\t" repo "/" f }')
  done | sort -u > "$IDX/imports.tsv"
  echo "index: $(wc -l < "$IDX/imports.tsv" | tr -d ' ') package import rows" >&2

  # 4. Exported names cited in module docs of this repository (AI.md, README.md,
  #    doc/**/*.md, ADRs excluded). documented.tsv: symbol <TAB> doc root <TAB>
  #    qualifier, where the doc root is the directory that owns the page (the
  #    parent of doc/) and the qualifier is the package prefix of `pkg.Symbol`.
  #    Only code-form citations count (`Symbol` or pkg.Symbol), never prose.
  git ls-files -- '*AI.md' '*README.md' '*/doc/*.md' '*/doc/**/*.md' | grep -vE 'node_modules|CHANGELOG|/adr/' | tr '\n' '\0' \
    | xargs -0 -r rg -H -o -N --no-heading '(`|\b[a-z][a-z0-9]*\.)[A-Z][A-Za-z0-9_]+\b' 2>/dev/null \
    | awk -v names="$IDX/names.txt" 'BEGIN { while ((getline n < names) > 0) want[n]=1 }
        { i = index($0, ":"); f = substr($0, 1, i-1); s = substr($0, i+1); q = ""; sub(/^`/, "", s)
          if ((j = index(s, ".")) > 0) { q = substr(s, 1, j-1); s = substr(s, j+1) }
          if (!(s in want)) next
          d = f; if (d ~ /\/doc\//) sub(/\/doc\/.*$/, "", d); else if (d ~ /\//) sub(/\/[^\/]*$/, "", d); else d = "."
          print s "\t" d "\t" q }' \
    | sort -u > "$IDX/documented.tsv"
  echo "index: $(wc -l < "$IDX/documented.tsv" | tr -d ' ') (exported name, doc root) citations in module docs" >&2
  date -u +%FT%TZ > "$IDX/indexed-at"
}

need_index() { [ -f "$IDX/indexed-at" ] || { echo "prune: run \`prune.sh index\` first" >&2; exit 2; }; }

# ---------------------------------------------------------------------------
# scans
# ---------------------------------------------------------------------------
# Count importers of a package outside its own directory, split into product
# code vs samples/tests. Zero product importers = dead; only samples/tests = a
# verdict for the owner.
report_package() { # file name dir extra-awk-match
  local n
  n="$(awk -F'\t' -v m="$2" -v d="$ROOT/$3/" '
        ($1 == m || '"$4"') && index($2, d) != 1 {
          if ($2 ~ /(_test\.go|\.test\.tsx?|\.spec\.tsx?)$|\/samples\/|\/templates\/|\/testdata\//) t++; else s++ }
        END { print s+0 "\t" t+0 }' "$IDX/imports.tsv")"
  local src="${n%%	*}" tst="${n##*	}"
  if [ "$src" -eq 0 ] && [ "$tst" -eq 0 ]; then
    emit dead "$1" '[]' "$2" 0 true "$2 is imported by nothing outside itself in this repository or its consumers"
  elif [ "$src" -eq 0 ]; then
    emit dead "$1" '[]' "$2" "$tst" false "$2 is imported only by samples, templates or tests ($tst sites): name the external consumer or delete it"
  fi
}

scan_dead() { # path...
  need_index
  local paths=("$@")
  # Packages: Go module paths and scoped npm names owned by the scanned paths.
  local scopes; scopes="$(package_scopes)"
  for p in "${paths[@]}"; do
    git ls-files -- "$p" | grep -E '(^|/)go\.mod$' | grep -vE '(^|/)(testdata|fixtures)/' | while read -r gm; do
      local mod dir n srcs
      mod="$(awk '/^module /{print $2; exit}' "$gm")"; dir="$(dirname "$gm")"
      # An extension or CLI binary is consumed through its manifest, not imported.
      srcs="$(git ls-files -- "$dir" | grep -E '\.go$' | grep -vE '_test\.go$')"
      if [ -n "$srcs" ] && printf '%s\n' "$srcs" | tr '\n' '\0' | xargs -0 -r grep -lqs '^package main' 2>/dev/null; then continue; fi
      report_package "$gm" "$mod" "$dir" 'index($1, m "/") == 1'

    done
    git ls-files -- "$p" | grep -E '(^|/)package\.json$' | grep -vE 'node_modules|testdata|fixtures' | while read -r pj; do
      local name dir n
      name="$(jq -r '.name // empty' "$pj")"; dir="$(dirname "$pj")"
      [ -n "$name" ] && printf '%s\n' "$scopes" | grep -qxF "${name%%/*}" || continue
      report_package "$pj" "$name" "$dir" '0'

    done
  done
  # Symbols: exported, no reference outside the defining package dir (src), or test-only.
  tracked src "${paths[@]}" | awk -F'\t' -v OFS='\t' 'NR==FNR { keep[$0]=1; next } ($2 in keep)' - "$IDX/defs.tsv" \
    | awk -F'\t' -v root="$ROOT/" -v refs="$IDX/refs.tsv" '
        BEGIN {
          while ((getline l < refs) > 0) {
            split(l, a, "\t"); sym=a[1]; f=a[2]; n=a[3]; t=a[4]
            d = f; sub(/\/[^\/]*$/, "", d)
            key = sym "\t" d
            if (t) tref[sym, d] += n; else sref[sym, d] += n
            dirs[sym] = dirs[sym] SUBSEP d
          }
        }
        {
          sym=$1; file=$2; pkg=root $3
          src=0; tst=0
          n = split(dirs[sym], ds, SUBSEP)
          for (i=1; i<=n; i++) { d=ds[i]; if (d=="" || d==pkg) continue
            src += sref[sym, d]; tst += tref[sym, d] }
          if (src == 0 && tst == 0) print "none\t" sym "\t" file
          else if (src == 0) print "test-only\t" sym "\t" file "\t" tst
        }' \
    | sort -u | awk -F'\t' -v docs="$IDX/documented.tsv" '
        # A doc cites the symbol when its root is the defining file'"'"'s directory
        # or one of its ancestors (a module documents its own exports only), and
        # its qualifier, if any, ends with the package directory name (`pgrpc.X`
        # cites grpc; `embed.FS` does not cite database).
        BEGIN { while ((getline l < docs) > 0) { split(l, a, "\t"); roots[a[1]] = roots[a[1]] "\n" a[2] "\t" a[3] } }
        {
          pkg = $3; sub(/\/[^\/]*$/, "", pkg); base = pkg; sub(/^.*\//, "", base)
          n = split(roots[$2], rs, "\n"); hit = 0
          for (i = 2; i <= n; i++) {
            split(rs[i], rq, "\t"); r = rq[1]; q = rq[2]
            if (q != "" && substr(q, length(q) - length(base) + 1) != base) continue
            if (r == "." || index($3, r "/") == 1) { hit = 1; break }
          }
          if (hit) { skipped++; next }
          print
        }
        END { if (skipped) printf "dead: %d exports skipped as named in module docs\n", skipped > "/dev/stderr" }' \
    | while IFS=$'\t' read -r kind sym file n; do
        # Skip Go main packages, test helpers and generated names.
        case "$file" in */cmd/*|*/main.go) continue ;; esac
        case "$sym" in Test*|Benchmark*|Example*|Fuzz*|Main|Register|New|Plugin) continue ;; esac
        ln="$(grep -nE "\b$sym\b" "$file" | head -1 | cut -d: -f1 || true)"
        if [ "$kind" = none ] && grep -qE "^func\s*\([^)]*\)\s*$sym\b" "$file"; then
          # A method with no caller by name may satisfy an interface: owner verdict.
          emit dead "$file" "[${ln:-0}]" "$sym" 0 false "exported method $sym is called by name nowhere outside its package: unexport it, or name the interface it satisfies"
        elif [ "$kind" = none ]; then
          emit dead "$file" "[${ln:-0}]" "$sym" 0 true "exported $sym is referenced nowhere outside its package in this repository or its consumers: unexport or delete"
        else
          emit dead "$file" "[${ln:-0}]" "$sym" "${n:-0}" false "exported $sym is referenced only from tests outside its package ($n refs): fold it into the test or unexport it"
        fi
      done
}

scan_duplicate() {
  local files
  files="$(tracked src "$@")"; [ -n "$files" ] || return 0
  printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 -r rg -H -N -o --no-heading \
      '^(func\s+|export\s+(async\s+)?function\s+)([a-zA-Z][A-Za-z0-9_]{5,})\s*[(<]' -r '$3' 2>/dev/null \
    | awk -F: '{ f=$1; s=$2; split(f, seg, "/"); p=seg[1] "/" seg[2] "/" seg[3]
                 key=s; if (index(seen[key], p "|")==0) { seen[key]=seen[key] p "|"; cnt[key]++ }
                 files[key]=files[key] f "," }
           END { for (k in cnt) if (cnt[k] >= 2) print cnt[k] "\t" k "\t" files[k] }' \
    | sort -rn | while IFS=$'\t' read -r n sym fl; do
        case "$sym" in main|String|Error|Close|Start|Stop|Run|Name|Validate|Register|Execute|Handle|Render|Parse|Marshal|Unmarshal) continue ;; esac
        first="${fl%%,*}"
        emit duplicate "$first" '[]' "$sym" "$n" false "function $sym is defined in $n projects: ${fl%,}"
      done
}

scan_multi_version() {
  local files
  files="$(tracked src "$@")"; [ -n "$files" ] || return 0
  printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 -r rg -H -n --no-heading -i \
      '\b(legacy|compat(ibility)?|fallback|deprecated|putnamirc|v1|v2|olderthan|migratefrom|upgradefrom)\b|Legacy[A-Z]|[a-z](V1|V2|Legacy|Compat|Fallback)\b' 2>/dev/null \
    | awk -F: '{ c[$1]++; if (!( $1 in first)) first[$1]=$2 } END { for (f in c) print c[f] "\t" f "\t" first[f] }' \
    | sort -rn | while IFS=$'\t' read -r n f ln; do
        emit multi-version "$f" "[$ln]" "$(basename "$f")" "$n" false "$n legacy/compat/fallback/v1-v2 markers: name the surviving path and delete the other"
      done
}

scan_palliative() {
  local src tst
  src="$(tracked src "$@")"; tst="$(tracked test "$@")"
  if [ -n "$src" ]; then
    printf '%s\n' "$src" | tr '\n' '\0' | xargs -0 -r rg -H -n --no-heading -i \
        '^\s*(//|\*|#).*\b(workaround|guard(s)? against|would otherwise|otherwise the|used to |historically|regressed|regression|poison|stale|hijack|race(d)? with|until (issue|PR|#))' 2>/dev/null \
      | awk -F: '{ c[$1]++; if (!($1 in first)) first[$1]=$2 } END { for (f in c) print c[f] "\t" f "\t" first[f] }' \
      | sort -rn | while IFS=$'\t' read -r n f ln; do
          emit palliative "$f" "[$ln]" "$(basename "$f")" "$n" false "$n guards explained by an incident: is the root cause still real? fix it or delete the guard"
        done
  fi
  if [ -n "$tst" ]; then
    printf '%s\n' "$tst" | tr '\n' '\0' | xargs -0 -r rg -H -n --no-heading \
        '"[0-9a-f]{9,40}"|"[0-9]+\.[0-9]+\.[0-9]+-[0-9a-f]{7,}"|sha256:[0-9a-f]{12,}' 2>/dev/null \
      | awk -F: '{ c[$1]++; if (!($1 in first)) first[$1]=$2 } END { for (f in c) print c[f] "\t" f "\t" first[f] }' \
      | sort -rn | while IFS=$'\t' read -r n f ln; do
          emit palliative "$f" "[$ln]" "$(basename "$f")" "$n" true "$n repository states (commit SHAs, stamped versions) pinned in a test: the test proves history, not a contract"
        done
  fi
}

scan_historical_ref() {
  local files
  files="$(git ls-files -- "$@" | grep -E '\.(go|ts|tsx|md|json|yaml|yml|sh)$' | grep -vE '(^|/)(node_modules|vendor|dist|\.gen)/|CHANGELOG\.md$|\.d\.ts$' || true)"
  [ -n "$files" ] || return 0
  printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 -r rg -H -n --no-heading \
      '(^|\s|\()#[0-9]{3,4}\b|\b(PR|pull request|issue) #?[0-9]{3,4}\b|\b[0-9a-f]{9}\b' 2>/dev/null \
    | grep -vE '^[^:]+:[0-9]+:\s*(-|\*|[0-9]+\.)?\s*\[#?[0-9]+\]' \
    | awk -F: '{ c[$1]++; if (!($1 in first)) first[$1]=$2 } END { for (f in c) print c[f] "\t" f "\t" first[f] }' \
    | sort -rn | while IFS=$'\t' read -r n f ln; do
        emit historical-ref "$f" "[$ln]" "$(basename "$f")" "$n" true "$n references to an issue, PR or commit: history belongs in the version control log; state the rule or delete the sentence"
      done
}

scan_comment() {
  local files
  files="$(tracked src "$@")"; [ -n "$files" ] || return 0
  # lint escapes
  printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 -r rg -H -n --no-heading \
      '//nolint|@ts-ignore|@ts-expect-error|\bas any\b|biome-ignore|eslint-disable' 2>/dev/null \
    | awk -F: '{ c[$1]++; if (!($1 in first)) first[$1]=$2 } END { for (f in c) print c[f] "\t" f "\t" first[f] }' \
    | sort -rn | while IFS=$'\t' read -r n f ln; do
        emit comment "$f" "[$ln]" "$(basename "$f")" "$n" false "$n lint escapes: change the code or change the rule, never annotate the wart"
      done
  # justifying comments
  printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 -r rg -H -n --no-heading -i \
      '^\s*(//|\*|#)\s.*\b(because the|so that the|this (keeps|guard|check) |would otherwise|otherwise the|we (must|need to|have to)|do not remove|don.t remove|keep this|needed (because|since)|required (because|since))\b' 2>/dev/null \
    | awk -F: '{ c[$1]++; if (!($1 in first)) first[$1]=$2 } END { for (f in c) print c[f] "\t" f "\t" first[f] }' \
    | sort -rn | while IFS=$'\t' read -r n f ln; do
        emit comment "$f" "[$ln]" "$(basename "$f")" "$n" true "$n comments justifying the code instead of describing its contract"
      done
  # comment density
  printf '%s\n' "$files" | while read -r f; do
    awk -v f="$f" '/^[[:space:]]*(\/\/|\*|\/\*)/ {c++} {t++} END { if (t > 80 && c*100/t > 30) printf "%d\t%s\t%d\n", c*100/t, f, t }' "$f"
  done | sort -rn | while IFS=$'\t' read -r pct f total; do
    emit comment "$f" '[]' "$(basename "$f")" "$pct" false "$pct % comment lines over $total: the prose is carrying what the code should say"
  done
}

scan_test_scaffold() {
  local all
  all="$(tracked all "$@")"; [ -n "$all" ] || return 0
  # test-only Go packages
  printf '%s\n' "$all" | grep -E '\.go$' | awk -F/ '{ d=$0; sub(/\/[^\/]*$/, "", d); if ($0 ~ /_test\.go$/) t[d]++; else s[d]++ }
      END { for (d in t) if (!(d in s)) print t[d] "\t" d }' \
    | sort -rn | while IFS=$'\t' read -r n d; do
        emit test-scaffold "$d" '[]' "$(basename "$d")" "$n" false "package holds $n test files and no source: it tests something else from outside, or it is scaffolding"
      done
  # helper / fake / mock files
  printf '%s\n' "$all" | grep -iE '(testutil|testenv|testsupport|testkit|fake|mock|stub|harness|fixture)[^/]*\.(go|ts|tsx)$' | while read -r f; do
    emit test-scaffold "$f" '[]' "$(basename "$f")" "$(wc -l < "$f" | tr -d ' ')" false "test double or harness: prefer the real component or the repository's own test support"
  done
  # test/source ratio per project directory (first three path segments)
  printf '%s\n' "$all" | while read -r f; do
    p="$(printf '%s' "$f" | awk -F/ '{print $1"/"$2"/"$3}')"
    n="$(wc -l < "$f" | tr -d ' ')"
    case "$f" in *_test.go|*.test.ts|*.test.tsx|*.spec.ts|*.spec.tsx) echo "t	$p	$n" ;; *) echo "s	$p	$n" ;; esac
  done | awk -F'\t' '{ if ($1=="t") t[$2]+=$3; else s[$2]+=$3 } END { for (p in t) if (s[p] > 0 && t[p] > 2*s[p]) printf "%d\t%s\t%d\t%d\n", t[p]*10/s[p], p, s[p], t[p] }' \
    | sort -rn | while IFS=$'\t' read -r r p s t; do
        emit test-scaffold "$p" '[]' "$(basename "$p")" "$((r))" false "test lines $t vs source $s (ratio $((r/10)).$((r%10))): tests are pinning implementation, not contract"
      done
}

scan_config_surface() {
  local files docs
  files="$(tracked src "$@")"; [ -n "$files" ] || return 0
  docs="$(git ls-files -- '*.md' | grep -vE 'node_modules|CHANGELOG' | tr '\n' '\0' | xargs -0 -r rg -o -N --no-filename 'PUTNAMI_[A-Z0-9_]+' 2>/dev/null | sort -u || true)"
  printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 -r rg -H -n -o --no-heading 'PUTNAMI_[A-Z0-9_]+' 2>/dev/null \
    | awk -F: '{ v=$3; if (!(v in first)) { first[v]=$1 ":" $2 }; c[v]++ } END { for (v in c) print c[v] "\t" v "\t" first[v] }' \
    | sort -rn | while IFS=$'\t' read -r n v loc; do
        f="${loc%%:*}"; ln="${loc##*:}"
        if printf '%s\n' "$docs" | grep -qx "$v"; then continue; fi
        emit config-surface "$f" "[$ln]" "$v" "$n" false "$v is read in code ($n sites) and documented nowhere: map it to a config key or delete it"
      done
}

scan_doc() {
  local md
  md="$(git ls-files -- "$@" | grep -E '\.md$' | grep -vE 'node_modules|CHANGELOG' || true)"
  [ -n "$md" ] || return 0
  # ADR status
  printf '%s\n' "$md" | grep -E '/adr/' | while read -r f; do
    st="$(rg -m1 -i -o -N --no-filename '^(\*\*)?status(\*\*)?:?\s*\**\s*(superseded|rejected|proposed|draft|deprecated)' "$f" 2>/dev/null | awk '{print tolower($NF)}' | tr -d '*' || true)"
    if [ -n "$st" ]; then emit doc "$f" '[1]' "$(basename "$f" .md)" 0 false "ADR is $st: keep only decisions that describe the released product; the rest is history"; fi
  done
  # dangling relative links
  printf '%s\n' "$md" | while read -r f; do
    d="$(dirname "$f")"
    rg -n -o --no-filename '\]\((\.{1,2}/[^)#? ]+|[a-zA-Z0-9_./-]+\.(md|go|ts|json))(#[^)]*)?\)' "$f" 2>/dev/null \
      | sed -E 's/^([0-9]+):\]\(([^)#]+).*/\1\t\2/' | while IFS=$'\t' read -r ln target; do
          case "$target" in http*|mailto*) continue ;; esac
          [ -e "$d/$target" ] || [ -e "$ROOT/${target#/}" ] || echo "$ln	$target"
        done | awk -F'\t' -v f="$f" '{ c++; if (c==1) { ln=$1; t=$2 } } END { if (c) print c "\t" f "\t" ln "\t" t }'
  done | sort -rn | while IFS=$'\t' read -r n f ln t; do
    emit doc "$f" "[$ln]" "$(basename "$f" .md)" "$n" true "$n links to paths that no longer exist (first: $t)"
  done
}

cmd_scan() {
  local typ="${1:?usage: prune.sh scan <typology|all> <path>...}"; shift
  # A scan with no match is a result, not a failure: grep/rg exit 1 on no hit.
  set +e +o pipefail
  [ "$#" -gt 0 ] || { echo "prune: scan needs at least one path" >&2; exit 2; }
  local paths=()
  for p in "$@"; do p="${p#/}"; paths+=("${p%/}"); done
  case "$typ" in
    dead) scan_dead "${paths[@]}" ;;
    duplicate) scan_duplicate "${paths[@]}" ;;
    multi-version) scan_multi_version "${paths[@]}" ;;
    palliative) scan_palliative "${paths[@]}" ;;
    historical-ref) scan_historical_ref "${paths[@]}" ;;
    comment) scan_comment "${paths[@]}" ;;
    test-scaffold) scan_test_scaffold "${paths[@]}" ;;
    config-surface) scan_config_surface "${paths[@]}" ;;
    doc) scan_doc "${paths[@]}" ;;
    all) for t in dead duplicate multi-version palliative historical-ref comment test-scaffold config-surface doc; do cmd_scan "$t" "${paths[@]}"; done ;;
    *) echo "prune: unknown typology $typ" >&2; exit 2 ;;
  esac
}

cmd="${1:-}"; [ -n "$cmd" ] || usage; shift
case "$cmd" in
  index) cmd_index ;;
  scan) cmd_scan "$@" ;;
  repos) cmd_repos ;;
  *) usage ;;
esac
