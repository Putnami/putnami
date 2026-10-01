#!/usr/bin/env bash
# english-only-proposals.sh against a real Putnami CLI and the shipped local
# provider, never a backend double. The Go harness that runs this script
# supplies both.
set -euo pipefail

# Under Git for Windows a native jq.exe ends every output line with CRLF, and
# the CR left in a captured value fails every comparison; --binary keeps LF.
case "${OSTYPE:-}" in
  msys* | cygwin*) jq() { command jq --binary "$@"; } ;;
esac

ROOT="$(git rev-parse --show-toplevel)"
SCANNER="$ROOT/.agents/skills/audit/scripts/english-only-proposals.sh"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

fail() {
  echo "english-only-proposals test: $*" >&2
  exit 1
}

: "${PUTNAMI_TEST_CLI:?this test needs PUTNAMI_TEST_CLI, a putnami binary}"
: "${PUTNAMI_TEST_LOCAL_PROVIDER:?this test needs PUTNAMI_TEST_LOCAL_PROVIDER, the local provider extension directory}"

# expect <status> <output-file> -- <scanner arguments...>, from the workspace.
expect() {
  local want="$1" out="$2" status=0
  shift 3
  (cd "$WS" && PATH="$WS/bin:$PATH" bash "$SCANNER" "$@") >"$out" 2>"$out.err" || status=$?
  [ "$status" -eq "$want" ] || {
    cat "$out" "$out.err" >&2
    fail "english-only-proposals.sh $* exited $status, want $want"
  }
}

WS="$TEST_DIR/workspace"
ORIGIN="$TEST_DIR/origin.git"
mkdir -p "$WS/bin"
git init -q --bare "$ORIGIN"
git init -q -b main "$WS"
git -C "$WS" config user.name "Skill Test"
git -C "$WS" config user.email "skill-test@example.com"
git -C "$WS" config commit.gpgsign false
cat >"$WS/putnami.workspace.json" <<JSON
{
  "name": "english-only-proposals",
  "extensions": ["$PUTNAMI_TEST_LOCAL_PROVIDER"],
  "options": {
    "collaboration": {
      "proposals": {"provider": "@putnami/local-collaboration", "version": 1, "settings": {"repository": "acme/app"}}
    }
  }
}
JSON
printf '.putnami/\nbin/\n' >"$WS/.gitignore"
ln -s "$PUTNAMI_TEST_CLI" "$WS/bin/putnami"
git -C "$WS" add -A
git -C "$WS" commit -q -m "chore: start"
git -C "$WS" remote add origin "$ORIGIN"
for branch in feat/french feat/english feat/draft feat/no-proposal feat/other-base release; do
  git -C "$WS" branch "$branch"
done
git -C "$WS" push -q origin --all
git -C "$WS" remote set-head origin main

# upsert <request-json>: record a proposal and print its id.
upsert() {
  (cd "$WS" && PATH="$WS/bin:$PATH" putnami proposals upsert --output=json --input "$1") |
    jq -er '.result.proposal.ref.id'
}
FRENCH="$(upsert '{"change":{"base":"main","head":"feat/french"},"title":"fix(cli): keep arrays","body":"Il faut garder les tableaux, donc nous changeons le parseur."}')"
ENGLISH="$(upsert '{"change":{"base":"main","head":"feat/english"},"title":"docs: explain runes","body":"A fixture like \"héllo\" is fine."}')"
DRAFT="$(upsert '{"change":{"base":"main","head":"feat/draft"},"title":"fix: ajoute le test","body":"Préserver les ordres sémantiques.","draft":true}')"
OTHER="$(upsert '{"change":{"base":"release","head":"feat/other-base"},"title":"fix: corrige le parseur","body":"Il faut, donc nous."}')"

# The base comes from origin/HEAD; the open and draft proposals into it are
# scanned, and nothing else is.
expect 1 "$TEST_DIR/scan.out" -- --limit 50
grep -Fq "proposal $FRENCH: non-English signals: donc, il faut, nous" "$TEST_DIR/scan.out" || fail "the French proposal was not reported"
grep -Fq "proposal $DRAFT: non-English signals" "$TEST_DIR/scan.out" || fail "a French draft proposal was not reported"
for skipped in "$ENGLISH" "$OTHER"; do
  if grep -Fq "proposal $skipped:" "$TEST_DIR/scan.out"; then
    cat "$TEST_DIR/scan.out" >&2
    fail "proposal $skipped was reported: an English proposal and one into another base are not scanned"
  fi
done
grep -Fq "2 of 3 proposal(s) into main are not in English" "$TEST_DIR/scan.out.err" || {
  cat "$TEST_DIR/scan.out.err" >&2
  fail "the summary does not count the scanned proposals"
}

# --base selects another base.
expect 1 "$TEST_DIR/release.out" -- --base release
grep -Fq "proposal $OTHER: non-English signals" "$TEST_DIR/release.out" || fail "--base release did not scan its proposal"

# --limit stops the scan.
expect 1 "$TEST_DIR/limit.out" -- --limit 1
[ "$(grep -c '^proposal [^:]*: non-English signals' "$TEST_DIR/limit.out")" -le 1 ] || fail "--limit 1 scanned more than one proposal"

expect 2 "$TEST_DIR/usage.out" -- --bogus
expect 2 "$TEST_DIR/limit-zero.out" -- --limit 0
expect 2 "$TEST_DIR/heads-zero.out" -- --heads 0

# --heads bounds the provider calls: one head asked, one find, and the summary
# counts the heads left out.
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" >>"%s"\nexec "%s" "$@"\n' "$TEST_DIR/finds" "$PUTNAMI_TEST_CLI" >"$TEST_DIR/counting-cli"
chmod +x "$TEST_DIR/counting-cli"
ln -sf "$TEST_DIR/counting-cli" "$WS/bin/putnami"
: >"$TEST_DIR/finds"
expect 0 "$TEST_DIR/heads.out" -- --base nowhere --heads 1
[ "$(grep -c '^proposals find' "$TEST_DIR/finds")" = 1 ] || fail "--heads 1 asked about more than one head"
grep -Fq "1 of 7 head(s) of origin asked, 0 unavailable, 6 beyond --heads 1" "$TEST_DIR/heads.out.err" || {
  cat "$TEST_DIR/heads.out.err" >&2
  fail "the summary does not count the heads beyond --heads"
}

# A head whose find answers unavailable is skipped and counted, and the scan
# goes on: offenders on the other heads are still reported.
cat >"$TEST_DIR/flaky-cli" <<STUB
#!/usr/bin/env bash
for head in \${UNAVAILABLE_HEADS:-}; do
  case "\$*" in
    *"\"head\":\"\$head\""*)
      printf '%s\n' '{"contract":"proposals","version":1,"operation":"find","outcome":"unavailable","error":{"message":"the provider did not answer","reason":"provider.timeout","retryable":true}}'
      exit 1
      ;;
  esac
done
exec "$PUTNAMI_TEST_CLI" "\$@"
STUB
chmod +x "$TEST_DIR/flaky-cli"
ln -sf "$TEST_DIR/flaky-cli" "$WS/bin/putnami"
export UNAVAILABLE_HEADS="feat/english"
expect 1 "$TEST_DIR/flaky.out" -- --base main
grep -Fq "proposal $FRENCH: non-English signals" "$TEST_DIR/flaky.out" || fail "an unavailable head stopped the scan of the others"
grep -Fq "skipping feat/english, unscanned" "$TEST_DIR/flaky.out.err" || fail "the unavailable head was not reported"
grep -Fq ", 1 unavailable," "$TEST_DIR/flaky.out.err" || fail "the summary does not count the unavailable head"
# Without an offender, an unavailable head leaves the scan incomplete: exit 2,
# never a clean scan.
export UNAVAILABLE_HEADS="feat/french feat/draft"
expect 2 "$TEST_DIR/incomplete.out" -- --base main
grep -Fq "no offender, but the scan is incomplete" "$TEST_DIR/incomplete.out.err" || fail "an incomplete scan was not reported as incomplete"
unset UNAVAILABLE_HEADS
ln -sf "$PUTNAMI_TEST_CLI" "$WS/bin/putnami"

# A workspace that binds no proposals provider is a tool failure, never a clean scan.
jq 'del(.options.collaboration.proposals) | .options.collaboration.tasks = {"provider": "@putnami/local-collaboration", "version": 1}' \
  "$WS/putnami.workspace.json" >"$TEST_DIR/unbound.json"
cp "$TEST_DIR/unbound.json" "$WS/putnami.workspace.json"
expect 2 "$TEST_DIR/unbound.out" -- --base main
grep -Fq "proposals find (main <- " "$TEST_DIR/unbound.out.err" || fail "an unbound proposals contract was not reported as a tool failure"

echo "english-only-proposals test: ok"
