#!/usr/bin/env bash
# Deterministic mechanical checks for /audit.
#
# These properties are lint rules, not judgment calls — running them through an
# LLM is nondeterministic and expensive. /audit runs this script once per
# project and files issues from its output; the model's judgment is reserved
# for triaging results (e.g. a CLI output writer legitimately printing to
# stdout) and for the semantic properties.
#
# Usage: mechanical.sh <project-path>        # e.g. mechanical.sh /go/framework/http
#
# Emits one JSON line per finding on stdout:
#   {"property": "...", "file": "...", "lines": [...], "value": N, "threshold": N, "message": "..."}
#
# Checks:
#   complexity-file-size      — Go files > 500 lines, TS files > 400 lines
#   complexity-nesting        — max indent depth >= 6 units (tabs, or 2-space)
#   complexity-dependencies   — internal import fan-out > 8 per file
#   observability-logging     — console.* (TS) / fmt.Print* & println (Go)
#   design-errors             — diag.Errorf/Warningf codes as string literals (Go)
#   design-stability          — a versioned protocols/* package (one shipping a
#                               fixtures/ corpus) must anchor its wire shape: a
#                               `const ProtocolVersion` in a .go source AND a
#                               `*_test.go` that references it (the conformance
#                               pin). Scoped to protocols/* only. A package may
#                               opt out with an explicit README marker
#                               `<!-- protocol-version: unversioned -->`.
#   security-timeout          — unbounded I/O paths (Go): a context.WithoutCancel
#                               not re-bounded by WithTimeout/WithDeadline/
#                               WithRequestTimeout, or an http.Client{} literal
#                               with no Timeout: field
#   design-errors-fmt-errorf  — bare fmt.Errorf in framework packages (Go)
#
# Test files, generated code (.gen/, *.d.ts), node_modules, dist, vendor, and
# testdata are excluded. Only git-tracked files are scanned.
set -euo pipefail

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

# test-contracts — every protocols/<pkg> that ships a JSON schema OR a strict
# parser must carry a non-empty fixtures/**/invalid/ corpus. The protocols
# README declares the valid/invalid fixture corpus the cross-language spec
# requires: a strict-parser reject branch with no invalid fixture lets a
# validation-rule relaxation pass the whole conformance suite unnoticed. This is
# a package-level property, so it runs once per project outside the file loop.
# Fixture files may be .json (most shapes) or .jsonl (runtime event streams);
# nesting is arbitrary (per-shape subdirs like cache/marker-lookup-result).
case "$rel" in
  protocols/*/*) : ;; # nested path (a sub-package); skip the package check
  protocols/*)
    schema_json="$(git ls-files -- "$rel/schemas" 2>/dev/null | grep -cE '\.json$' || true)"
    has_schema=no; [ "${schema_json:-0}" -gt 0 ] && has_schema=yes
    has_strict=no
    if [ "$has_schema" = no ]; then
      # No schema dir — qualify on a strict parser (DisallowUnknownFields or the
      # shared parseStrict/ParseStrict helper) in non-test Go source.
      strict_src="$(git ls-files -- "$rel" \
        | grep -E '\.go$' | grep -vE '(_test\.go)$' || true)"
      strict_hits=0
      if [ -n "$strict_src" ]; then
        strict_hits="$(printf '%s\n' "$strict_src" | tr '\n' '\0' \
          | xargs -0 grep -lE 'DisallowUnknownFields|parseStrict|ParseStrict' 2>/dev/null \
          | grep -c . || true)"
      fi
      [ "${strict_hits:-0}" -gt 0 ] && has_strict=yes
    fi
    if [ "$has_schema" = yes ] || [ "$has_strict" = yes ]; then
      n_invalid="$(git ls-files -- "$rel/fixtures" 2>/dev/null \
        | grep -cE '/invalid/[^/]+\.(json|jsonl)$' || true)"
      if [ "${n_invalid:-0}" -eq 0 ]; then
        emit test-contracts "$rel" 0 1 \
          "schema/strict-parser package has no fixtures/**/invalid/ corpus — a reject branch can regress untested" '[]'
      fi
    fi
    ;;
esac

files="$(git ls-files -- "$rel" \
  | grep -E '\.(go|ts|tsx)$' \
  | grep -vE '(^|/)(node_modules|dist|testdata|vendor|\.gen)/' \
  | grep -vE '(_test\.go|\.test\.tsx?|\.spec\.tsx?|\.d\.ts)$' || true)"

[ -n "$files" ] || { echo "mechanical: no source files in $rel" >&2; exit 0; }

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
    fanout="$(grep -cE '^[[:space:]]*([[:alnum:]_]+ )?"go\.putnami\.dev/' "$f" || true)"
  else
    fanout="$(grep -cE "from ['\"](\.|@putnami/)" "$f" || true)"
  fi
  if [ "$fanout" -gt 8 ]; then
    emit complexity-dependencies "$f" "$fanout" 8 \
      "$fanout internal imports (fan-out threshold 8)" '[]'
    count_findings=$((count_findings + 1))
  fi

  # observability-logging — raw prints instead of the structured logger
  if [ "$lang" = go ]; then
    pattern='\bfmt\.Print(ln|f)?\(|^[[:space:]]*println\('
  else
    pattern='\bconsole\.(log|warn|error|info|debug|trace)\('
  fi
  hits="$(grep -nE "$pattern" "$f" | cut -d: -f1 || true)"
  if [ -n "$hits" ]; then
    nhits="$(printf '%s\n' "$hits" | wc -l | tr -d ' ')"
    emit observability-logging "$f" "$nhits" 0 \
      "$nhits raw print/console call(s) — framework code must use the structured logger" \
      "$(printf '%s\n' "$hits" | lines_json)"
    count_findings=$((count_findings + 1))
  fi

  # design-errors — diagnostic codes must be exported, namespaced ErrorCode*
  # constants, not ad-hoc string literals. Purely syntactic: a diag.Errorf/
  # Warningf call whose code argument (the first) is a string literal ("...")
  # rather than an identifier. The diagnostic package is aliased `diag`
  # everywhere; fmt.Errorf and t.Errorf are deliberately not matched.
  if [ "$lang" = go ]; then
    codes="$(grep -nE '\bdiag\.(Errorf|Warningf)\("' "$f" | cut -d: -f1 || true)"
    if [ -n "$codes" ]; then
      ncodes="$(printf '%s\n' "$codes" | wc -l | tr -d ' ')"
      emit design-errors "$f" "$ncodes" 0 \
        "$ncodes diagnostic code(s) passed to diag.Errorf/Warningf as string literals — use exported, namespaced ErrorCode* constants (pkg.snake_case)" \
        "$(printf '%s\n' "$codes" | lines_json)"
      count_findings=$((count_findings + 1))
    fi
  fi

  # security-timeout — unbounded / mis-bounded request I/O paths (Go only).
  # Two mechanically-detectable shapes are flagged:
  #   (a) a context.WithoutCancel( that is NOT re-bounded by a WithTimeout/
  #       WithDeadline/WithRequestTimeout within a 3-line window (the call, the
  #       line above, the line below). Detaching a context strips the request
  #       deadline, so a leader running on it must re-install one or a hung
  #       dependency wedges every waiter. A WithoutCancel passed inline as an
  #       argument to a best-effort call on the same line (there is a "(" before
  #       it) is a discarded cleanup/rollback and is not flagged.
  #   (b) an http.Client{ composite literal that sets other fields but no
  #       Timeout: before its matching closing brace. The empty zero-value
  #       literal http.Client{} is exempt: it is the deliberate "no whole-
  #       exchange timeout so a streaming Get is not aborted mid-download"
  #       form, whose per-operation deadlines come from WithRequestTimeout on
  #       the request context (see go/framework/storage/backend.go).
  if [ "$lang" = go ]; then
    timeouts="$(awk '
      function has_rebind(s) {
        return (s ~ /context\.WithTimeout\(/ || s ~ /context\.WithDeadline\(/ || s ~ /WithRequestTimeout\(/)
      }
      { line[NR] = $0 }
      END {
        for (i = 1; i <= NR; i++) {
          s = line[i]

          # (a) detached context not re-bounded.
          if (s ~ /context\.WithoutCancel\(/) {
            # Inline best-effort argument: a "(" precedes the call on this line.
            before = s
            sub(/context\.WithoutCancel\(.*/, "", before)
            inline_arg = (before ~ /\(/)
            rebound = has_rebind(s) || has_rebind(line[i-1]) || has_rebind(line[i+1])
            if (!inline_arg && !rebound) print i ":withoutcancel"
          }

          # (b) http.Client composite literal with no Timeout: field. The
          # empty zero-value literal http.Client{} is the deliberate
          # streaming form and is exempt.
          if (s ~ /http\.Client\{/ && s !~ /http\.Client\{\}/) {
            rest = s
            sub(/.*http\.Client\{/, "", rest)     # text after the opening brace
            depth = 1
            j = i
            seg = rest
            found = (seg ~ /Timeout:/)
            # Walk to the matching closing brace, scanning each segment for Timeout:.
            while (depth > 0 && j <= NR) {
              n = length(seg)
              for (k = 1; k <= n; k++) {
                c = substr(seg, k, 1)
                if (c == "{") depth++
                else if (c == "}") { depth--; if (depth == 0) break }
              }
              if (depth == 0) break
              j++
              seg = line[j]
              if (seg ~ /Timeout:/) found = 1
            }
            if (!found) print i ":httpclient"
          }
        }
      }
    ' "$f")"
    wc_hits="$(printf '%s' "$timeouts" | grep -c ':withoutcancel$' || true)"
    hc_hits="$(printf '%s' "$timeouts" | grep -c ':httpclient$' || true)"
    if [ -n "$timeouts" ]; then
      if [ "$wc_hits" -gt 0 ]; then
        emit security-timeout "$f" "$wc_hits" 0 \
          "$wc_hits context.WithoutCancel call(s) not re-bounded by a WithTimeout/WithDeadline/WithRequestTimeout — a detached context strips the request deadline and can wedge waiters" \
          "$(printf '%s\n' "$timeouts" | grep ':withoutcancel$' | cut -d: -f1 | lines_json)"
        count_findings=$((count_findings + 1))
      fi
      if [ "$hc_hits" -gt 0 ]; then
        emit security-timeout "$f" "$hc_hits" 0 \
          "$hc_hits http.Client{} literal(s) with no Timeout: field — an unbounded client can hang forever on a stalled server" \
          "$(printf '%s\n' "$timeouts" | grep ':httpclient$' | cut -d: -f1 | lines_json)"
        count_findings=$((count_findings + 1))
      fi
    fi
  fi

  # design-errors-fmt-errorf — framework code must construct errors with the
  # workspace's structured go.putnami.dev/errors (coded Error), not bare
  # fmt.Errorf, so callers can match by a stable Code. Purely syntactic: any
  # fmt.Errorf( call is a finding.
  #
  # /audit runs mechanical.sh per project path and scopes this script's callers
  # to role/framework projects, so this check applies to framework packages.
  # role/extension and role/sample are intentionally exempt (extension errors
  # surface via emit.Diagnostic, samples are illustrative), and _test.go files
  # are already excluded by the file filter above.
  if [ "$lang" = go ]; then
    errorf="$(grep -nE '\bfmt\.Errorf\(' "$f" | cut -d: -f1 || true)"
    if [ -n "$errorf" ]; then
      nerrorf="$(printf '%s\n' "$errorf" | wc -l | tr -d ' ')"
      emit design-errors-fmt-errorf "$f" "$nerrorf" 0 \
        "$nerrorf bare fmt.Errorf call(s) — framework code must use structured errors (go.putnami.dev/errors New/Newf/Wrap/Wrapf with a namespaced Code) so callers can match by Code" \
        "$(printf '%s\n' "$errorf" | lines_json)"
      count_findings=$((count_findings + 1))
    fi
  fi
done <<EOF
$files
EOF

# design-stability — protocols/* packages only. A versioned contract must anchor
# its wire shape in a checkable place: a `const ProtocolVersion` declared in a
# non-test .go source AND a `*_test.go` that references it (the conformance pin
# every reference package carries, e.g. protocols/registry/conformance_test.go).
# Without the anchor a bump is invisible — a dead const is a stale anchor, and an
# unpinned const drifts silently. Scoped to packages that ship a fixtures/ corpus
# (the signal that the package is a real, exercised contract). A package that is
# deliberately not yet versioned opts out with an explicit README marker
# `<!-- protocol-version: unversioned -->` so the exemption is documented in the
# tree, not hidden in this script.
case "$rel" in
  protocols/*)
    pkgdir="${rel%/}"
    # Only a package with a fixtures corpus is in scope. Match protocols/<pkg>
    # exactly (a fixtures/ subtree, not the audited path being a fixture itself).
    if [ -d "$pkgdir/fixtures" ]; then
      readme="$pkgdir/README.md"
      opted_out=0
      if [ -f "$readme" ] && grep -q '<!-- protocol-version: unversioned -->' "$readme"; then
        opted_out=1
      fi
      if [ "$opted_out" -eq 0 ]; then
        # A non-test .go source that declares `const ProtocolVersion`.
        const_src="$(git ls-files -- "$pkgdir/*.go" \
          | grep -vE '(_test\.go)$' \
          | xargs -r grep -lE '^[[:space:]]*const[[:space:]]+ProtocolVersion[[:space:]]*=' 2>/dev/null \
          | head -1 || true)"
        # A *_test.go that references ProtocolVersion (the conformance pin).
        test_ref="$(git ls-files -- "$pkgdir/*_test.go" \
          | xargs -r grep -lE '\bProtocolVersion\b' 2>/dev/null \
          | head -1 || true)"
        if [ -z "$const_src" ]; then
          emit design-stability "$pkgdir" 0 1 \
            "versioned protocol package ships fixtures/ but declares no 'const ProtocolVersion' anchor — add one and pin it in a TestConformance_ProtocolVersion (see protocols/registry/conformance_test.go), or opt out with a '<!-- protocol-version: unversioned -->' README marker" \
            '[]'
          count_findings=$((count_findings + 1))
        elif [ -z "$test_ref" ]; then
          emit design-stability "$const_src" 0 1 \
            "'const ProtocolVersion' is declared but never referenced by any *_test.go — pin it in a TestConformance_ProtocolVersion so a bump requires a migration story (see protocols/registry/conformance_test.go)" \
            '[]'
          count_findings=$((count_findings + 1))
        fi
      fi
    fi
    ;;
esac

echo "mechanical: scanned $count_files files in $rel, $count_findings findings" >&2
