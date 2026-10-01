#!/usr/bin/env bash
set -euo pipefail

# Under Git for Windows a native jq.exe ends every output line with CRLF, and
# the CR left in a captured value fails every comparison; --binary keeps LF.
case "${OSTYPE:-}" in
  msys* | cygwin*) jq() { command jq --binary "$@"; } ;;
esac

ROOT="$(git rev-parse --show-toplevel)"
DETECTOR="$ROOT/.agents/skills/check/scripts/english-only.sh"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

fail() {
  echo "english-only test: $*" >&2
  exit 1
}

# expect <status> <output-file> -- <detector arguments...>; stdin is forwarded.
expect() {
  local want="$1" out="$2" status=0
  shift 3
  bash "$DETECTOR" "$@" >"$out" 2>"$out.err" || status=$?
  [ "$status" -eq "$want" ] || {
    cat "$out" "$out.err" >&2
    fail "english-only.sh $* exited $status, want $want"
  }
}

# --- text mode --------------------------------------------------------------

cat >"$TEST_DIR/french-words.md" <<'BODY'
## Summary

Il faut garder les tableaux, donc nous changeons le parseur.
BODY
expect 1 "$TEST_DIR/french-words.out" -- text "$TEST_DIR/french-words.md"
grep -Fq "$TEST_DIR/french-words.md: non-English signals: donc, il faut, nous" "$TEST_DIR/french-words.out" ||
  fail "french function words were not listed"
grep -Fq "$TEST_DIR/french-words.md:3:Il faut garder" "$TEST_DIR/french-words.out" ||
  fail "offending line number was not listed"

# Two distinct accented words and no listed function word still fire.
printf 'Advances #1 ("préserver les ordres sémantiques").\n' >"$TEST_DIR/accents.md"
expect 1 "$TEST_DIR/accents.out" -- text --label "PR title and body" "$TEST_DIR/accents.md"
grep -Fq "PR title and body: non-English signals: préserver, sémantiques" "$TEST_DIR/accents.out" ||
  fail "accented words were not listed under the label"

# Stdin is the default input.
printf 'Ajoute le test avec la fixture.\n' | expect 1 "$TEST_DIR/stdin.out" -- text
grep -Fq "stdin: non-English signals: avec" "$TEST_DIR/stdin.out" || fail "stdin unit was not labeled"

cat >"$TEST_DIR/english.md" <<'BODY'
## Summary

- Count runes, not bytes: a 5-rune string like "héllo" is 6 bytes.
- Quote fixtures as code: `s3cr3t-éà-value` and `déjà-vu` stay allowed.
- The RGPD (Règlement Général sur la Protection des Données) and the CNIL
  (Commission Nationale de l'Informatique et des Libertés) keep their names.
- Words that merely contain a listed word, like enormous or avocado, pass.

```go
const fixture = "garder les données avec soin"
```
BODY
expect 0 "$TEST_DIR/english.out" -- text "$TEST_DIR/english.md"
[ ! -s "$TEST_DIR/english.out" ] || fail "English text produced a listing"

# Two distinct common French words fire even without an accent or a phrase.
printf 'chore: supprime les fichiers inutiles dans la CI\n' | expect 1 "$TEST_DIR/common.out" -- text --label subject
grep -Fq "subject: non-English signals: dans, fichiers, la, les, supprime" "$TEST_DIR/common.out" ||
  fail "common French words were not listed"
printf 'fix(cli): corrige le parseur des tableaux\n' | expect 1 "$TEST_DIR/common-short.out" -- text

# One common word that is also English, acronyms and command-line options do not.
cat >"$TEST_DIR/english-common.md" <<'BODY'
fix(cli): keep the DES and EST constants for the Des Moines office

Run ls -la, then compare with [ "$a" -le "$b" ] in the same shell.
The RGPD (Règlement Général sur la Protection
des Données) wraps across two lines here.
BODY
expect 0 "$TEST_DIR/english-common.out" -- text "$TEST_DIR/english-common.md"

expect 2 "$TEST_DIR/usage.out" -- bogus
expect 2 "$TEST_DIR/missing.out" -- text "$TEST_DIR/does-not-exist.md"

# --- files mode -------------------------------------------------------------

REPO="$TEST_DIR/repo"
git init -b main "$REPO" >/dev/null
git -C "$REPO" config user.name "Skill Test"
git -C "$REPO" config user.email "skill-test@example.com"
mkdir -p "$REPO/pkg/testdata" "$REPO/docs"
printf 'package pkg\n\n// Parse reads the value.\nfunc Parse() {}\n' >"$REPO/pkg/parse.go"
printf 'package pkg\n\n// Il faut garder les données.\nconst x = 1\n' >"$REPO/pkg/french.go"
printf 'package pkg\n\nconst fixture = "garder les données avec soin"\n' >"$REPO/pkg/parse_test.go"
printf 'garder les données avec soin\n' >"$REPO/pkg/testdata/input.txt"
printf '# Guide\n\nThe word "héllo" is a fixture.\n' >"$REPO/docs/guide.md"
git -C "$REPO" add -A
git -C "$REPO" commit -m "feat: add the parser" >/dev/null

(cd "$REPO/docs" && expect 1 "$TEST_DIR/files.out" -- files)
grep -Fq "pkg/french.go: non-English signals: il faut" "$TEST_DIR/files.out" || fail "french.go was not reported"
grep -Fq "pkg/french.go:3:// Il faut garder" "$TEST_DIR/files.out" || fail "french.go line was not reported"
if grep -Eq "parse_test.go|testdata|guide.md|parse.go:" "$TEST_DIR/files.out"; then
  cat "$TEST_DIR/files.out" >&2
  fail "an excluded or English file was reported"
fi

# Pathspecs narrow the scan and resolve from the caller's directory, as in git.
(cd "$REPO" && expect 0 "$TEST_DIR/files-docs.out" -- files docs)
(cd "$REPO/pkg" && expect 1 "$TEST_DIR/files-relative.out" -- files french.go)
grep -Fq "pkg/french.go: non-English signals" "$TEST_DIR/files-relative.out" || fail "a relative pathspec scanned nothing"

# --diff scans only the files a range adds or modifies, and nothing when it has none.
printf '# Notes\n\nNothing to report.\n' >"$REPO/docs/notes.md"
git -C "$REPO" add docs/notes.md
git -C "$REPO" commit -m "docs: add notes" >/dev/null
# pkg/french.go is still tracked, so a clean result means only notes.md was read.
(cd "$REPO" && expect 0 "$TEST_DIR/diff-english.out" -- files --diff HEAD~1..HEAD)
(cd "$REPO" && expect 0 "$TEST_DIR/diff-empty.out" -- files --diff HEAD..HEAD)
grep -Fq "0 scanned unit(s), no offender" "$TEST_DIR/diff-empty.out.err" || fail "an empty --diff scanned files"
EMPTY_TREE="$(git -C "$REPO" hash-object -t tree /dev/null)"
(cd "$REPO" && expect 1 "$TEST_DIR/diff-french.out" -- files --diff "$EMPTY_TREE..HEAD")
grep -Fq "pkg/french.go: non-English signals" "$TEST_DIR/diff-french.out" || fail "--diff missed a changed French file"
(cd "$REPO" && expect 2 "$TEST_DIR/diff-bad.out" -- files --diff no-such-revision)

# --- commits mode -----------------------------------------------------------

git -C "$REPO" rm -q pkg/french.go
git -C "$REPO" commit -m "fix: supprime le fichier, donc le test passe" >/dev/null
(cd "$REPO" && expect 0 "$TEST_DIR/files-clean.out" -- files)
(cd "$REPO" && expect 1 "$TEST_DIR/commits.out" -- commits HEAD~1..HEAD)
grep -Eq "^commit [0-9a-f]+: non-English signals: donc" "$TEST_DIR/commits.out" || fail "french commit was not reported"
(cd "$REPO" && expect 0 "$TEST_DIR/commits-clean.out" -- commits HEAD~1)
(cd "$REPO" && expect 2 "$TEST_DIR/commits-missing.out" -- commits)

# --- tasks mode -------------------------------------------------------------

# The tasks mode reads tasks through the collaboration contract, so it runs
# against a real Putnami CLI and the shipped local provider, never a backend
# double. The Go harness that runs this script supplies both.
: "${PUTNAMI_TEST_CLI:?the tasks mode test needs PUTNAMI_TEST_CLI, a putnami binary}"
: "${PUTNAMI_TEST_LOCAL_PROVIDER:?the tasks mode test needs PUTNAMI_TEST_LOCAL_PROVIDER, the local provider extension directory}"
TASKS_WS="$TEST_DIR/tasks-workspace"
mkdir -p "$TASKS_WS/bin"
git init -q "$TASKS_WS"
cat >"$TASKS_WS/putnami.workspace.json" <<JSON
{
  "name": "english-only-tasks",
  "extensions": ["$PUTNAMI_TEST_LOCAL_PROVIDER"],
  "options": {
    "collaboration": {
      "tasks": {"provider": "@putnami/local-collaboration", "version": 1}
    }
  }
}
JSON
ln -s "$PUTNAMI_TEST_CLI" "$TASKS_WS/bin/putnami"
# create_task <request-json>: record a task and print its id.
create_task() {
  (cd "$TASKS_WS" && PATH="$TASKS_WS/bin:$PATH" putnami tasks create --output=json --input "$1") |
    jq -er '.result.task.ref.id'
}
FRENCH_TASK="$(create_task '{"title":"fix(cli): keep arrays","body":"Il faut garder les tableaux, donc nous changeons le parseur.","idempotencyKey":"english-only:french"}')"
ENGLISH_TASK="$(create_task '{"title":"docs: explain runes","body":"A fixture like \"héllo\" is fine.","idempotencyKey":"english-only:english"}')"
TRACKER_TASK="$(create_task '{"title":"[workspace] Non-English text","body":"task 7: donc\n\naudit-fp: workspace/language/english-only","idempotencyKey":"english-only:tracker"}')"
DONE_TASK="$(create_task '{"title":"fix: ajoute le test","body":"Préserver les ordres sémantiques.","state":"done","idempotencyKey":"english-only:done"}')"
(cd "$TASKS_WS" && PATH="$TASKS_WS/bin:$PATH" expect 1 "$TEST_DIR/tasks.out" -- tasks)
grep -Fq "task $FRENCH_TASK: non-English signals: donc, il faut, nous" "$TEST_DIR/tasks.out" || fail "a French task was not reported"
for skipped in "$ENGLISH_TASK" "$TRACKER_TASK" "$DONE_TASK"; do
  if grep -Fq "task $skipped:" "$TEST_DIR/tasks.out"; then
    cat "$TEST_DIR/tasks.out" >&2
    fail "task $skipped was reported: an English task, the tracking task and a done task are not scanned"
  fi
done
# The deprecated github spelling runs the same scan and says what it no longer covers.
(cd "$TASKS_WS" && PATH="$TASKS_WS/bin:$PATH" expect 1 "$TEST_DIR/github.out" -- github)
grep -Fq "task $FRENCH_TASK: non-English signals" "$TEST_DIR/github.out" || fail "the github alias did not scan tasks"
grep -Fq "deprecated alias of the tasks mode" "$TEST_DIR/github.out.err" || fail "the github alias did not announce its deprecation"
(cd "$TASKS_WS" && PATH="$TASKS_WS/bin:$PATH" expect 2 "$TEST_DIR/tasks-repo.out" -- tasks --repo example/repo)
grep -Fq -- "--repo is not supported" "$TEST_DIR/tasks-repo.out.err" || fail "--repo was not refused"
# A workspace that binds no tasks provider is a tool failure, never a clean scan.
jq 'del(.options.collaboration.tasks) | .options.collaboration.proposals = {"provider": "@putnami/local-collaboration", "version": 1}' \
  "$TASKS_WS/putnami.workspace.json" >"$TEST_DIR/unbound.json"
cp "$TEST_DIR/unbound.json" "$TASKS_WS/putnami.workspace.json"
(cd "$TASKS_WS" && PATH="$TASKS_WS/bin:$PATH" expect 2 "$TEST_DIR/tasks-unbound.out" -- tasks)
grep -Fq "tasks find answered unsupported" "$TEST_DIR/tasks-unbound.out.err" || fail "an unbound tasks contract was not reported as a tool failure"

# --- check's proposal-text step ---------------------------------------------

# The step /check runs on the branch's proposal, taken from the shipped
# instructions as they are written: it checks the text only when the proposals
# contract answers ok with a proposal, and says NOT CHECKED otherwise, never
# a clean scan.
STEP="$TEST_DIR/check-proposal-step.sh"
sed -n '/^envelope="\$("\$PUTNAMI_CLI" proposals find/,/^fi$/p' "$ROOT/.agents/skills/check/SKILL.md" |
  sed -e 's/<base>/main/g' -e 's#<branch>#feat/french#g' \
    -e "s#bash .agents/skills/check/scripts/english-only.sh#bash '$DETECTOR'#" >"$STEP"
grep -Fq "english-only.sh' text" "$STEP" || fail "check's proposal-text step is missing from its instructions"
# run_step <status> <output-file>: the step, from the workspace.
run_step() {
  local want="$1" out="$2" status=0
  (cd "$TASKS_WS" && PATH="$TASKS_WS/bin:$PATH" PUTNAMI_CLI=putnami bash -o pipefail "$STEP") >"$out" 2>"$out.err" || status=$?
  [ "$status" -eq "$want" ] || {
    cat "$out" "$out.err" >&2
    fail "check's proposal-text step exited $status, want $want"
  }
}
jq '.options.collaboration.proposals.settings = {"repository": "acme/app"}' "$TASKS_WS/putnami.workspace.json" >"$TEST_DIR/proposals.json"
cp "$TEST_DIR/proposals.json" "$TASKS_WS/putnami.workspace.json"
run_step 0 "$TEST_DIR/step-none.out"
grep -Fxq "proposal title and body: NOT CHECKED (outcome ok, 0 proposal(s))" "$TEST_DIR/step-none.out" ||
  fail "a branch without a proposal was not reported as not checked"
(cd "$TASKS_WS" && PATH="$TASKS_WS/bin:$PATH" putnami proposals upsert --output=json \
  --input '{"change":{"base":"main","head":"feat/french"},"title":"fix(cli): keep arrays","body":"Il faut garder les tableaux, donc nous changeons le parseur."}' >/dev/null)
run_step 1 "$TEST_DIR/step-french.out"
grep -Fq "proposal title and body: non-English signals: donc, il faut, nous" "$TEST_DIR/step-french.out" ||
  fail "a French proposal was not reported"
jq 'del(.options.collaboration.proposals)' "$TASKS_WS/putnami.workspace.json" >"$TEST_DIR/no-proposals.json"
cp "$TEST_DIR/no-proposals.json" "$TASKS_WS/putnami.workspace.json"
run_step 0 "$TEST_DIR/step-unbound.out"
grep -Fxq "proposal title and body: NOT CHECKED (outcome unsupported, 0 proposal(s))" "$TEST_DIR/step-unbound.out" ||
  fail "an unbound proposals contract was not reported as not checked"
if grep -q "no offender" "$TEST_DIR/step-unbound.out" "$TEST_DIR/step-unbound.out.err"; then
  fail "an unbound proposals contract was scanned as a clean text"
fi

echo "english-only test: ok"
