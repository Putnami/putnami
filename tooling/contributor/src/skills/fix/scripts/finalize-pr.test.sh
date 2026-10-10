#!/usr/bin/env bash
set -euo pipefail

# Every variable that selects or configures a git repository from outside is
# unset, so git finds this repository from the working directory and each
# scratch repository from its own path.
git_env_vars="$(git rev-parse --local-env-vars)"
while IFS= read -r name; do unset "${name%$'\r'}"; done <<<"$git_env_vars"
unset GIT_NAMESPACE GIT_CEILING_DIRECTORIES GIT_DISCOVERY_ACROSS_FILESYSTEM

ROOT="$(git rev-parse --show-toplevel)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

# Tasks and proposals go through a real Putnami CLI to the shipped local
# provider: the only doubles are the gate process and the answers injected at
# the CLI boundary below. The Go harness that runs this script builds both.
: "${PUTNAMI_TEST_CLI:?the finalizer test needs PUTNAMI_TEST_CLI, a putnami binary}"
: "${PUTNAMI_TEST_LOCAL_PROVIDER:?the finalizer test needs PUTNAMI_TEST_LOCAL_PROVIDER, the local provider extension directory}"
: "${PUTNAMI_TEST_UPSERT_RECONCILE:?the finalizer test needs PUTNAMI_TEST_UPSERT_RECONCILE, the reconcile hint the CLI writes on an unresolved proposals.upsert}"

# The finalizer runs from a copy of the shipped scripts, and every copy below
# sits under TEST_DIR, whose putnamiw starts the CLI the Go harness built from
# this tree. Its tree fingerprint is then the CLI's, without the staleness
# check this repository's own putnamiw runs over the CLI sources first: the
# suite fingerprints the tree about 150 times, and on a loaded machine that
# check alone outlasted `go test`'s limit. TestTreeFingerprint runs the script
# through the real wrapper.
mkdir -p "$TEST_DIR/shipped/skills/fix/scripts" "$TEST_DIR/shipped/skills/check/scripts"
for script in finalize-pr.sh tree-fingerprint.sh machine-load.sh; do
  cp "$ROOT/.agents/skills/fix/scripts/$script" "$TEST_DIR/shipped/skills/fix/scripts/$script"
done
cp "$ROOT/.agents/skills/check/scripts/english-only.sh" "$TEST_DIR/shipped/skills/check/scripts/english-only.sh"
FINALIZER="$TEST_DIR/shipped/skills/fix/scripts/finalize-pr.sh"
printf '#!/usr/bin/env bash\nexec "${PUTNAMI_TEST_CLI:?}" "$@"\n' >"$TEST_DIR/putnamiw"
chmod +x "$TEST_DIR/putnamiw"

mkdir -p "$TEST_DIR/bin" "$TEST_DIR/state"
# Both generated hosts must defer ownership and naming to the consumer graph.
for analyst in "$ROOT/.claude/agents/epic-analyst.md" "$ROOT/.codex/agents/epic-analyst.toml"; do
  grep -Fq 'AGENTS.md' "$analyst"
  grep -Fq "The consumer repository's authoritative rules define its topology and naming." "$analyst"
  grep -Fq 'Resolve actual project IDs, owners, dependencies, and documentation paths from the workspace graph' "$analyst"
  if grep -Eq 'language-first|go\.putnami\.dev/<name>' "$analyst"; then
    echo "finalize-pr test: analyst imposes the producer topology: $analyst" >&2
    exit 1
  fi
done

# Run the selectors copied into each host's shipped instructions. They must
# work in a wrapper-free consumer and prefer an executable local wrapper.
mkdir -p "$TEST_DIR/selector"
check_cli_selector() {
  local instructions="$1" selector selected
  selector="$(sed -n '/^PUTNAMI_CLI=putnami$/,/^fi$/p' "$instructions")"
  [ -n "$selector" ] || { echo "missing CLI selector: $instructions" >&2; exit 1; }
  if grep -Eq '\./putnamiw (lint|test|build|projects)' "$instructions"; then
    echo "unconditional wrapper command: $instructions" >&2
    exit 1
  fi
  selected="$(cd "$TEST_DIR/selector" && eval "$selector" && printf '%s' "$PUTNAMI_CLI")"
  [ "$selected" = putnami ]
  touch "$TEST_DIR/selector/putnamiw"
  selected="$(cd "$TEST_DIR/selector" && eval "$selector" && printf '%s' "$PUTNAMI_CLI")"
  [ "$selected" = putnami ]
  chmod +x "$TEST_DIR/selector/putnamiw"
  selected="$(cd "$TEST_DIR/selector" && eval "$selector" && printf '%s' "$PUTNAMI_CLI")"
  [ "$selected" = ./putnamiw ]
  rm "$TEST_DIR/selector/putnamiw"
}
for host in .agents .claude; do
  for skill in fix execute epic plan check code-review content-bump fix-loop putnami-change putnami-check putnami-plan putnami-review; do
    check_cli_selector "$ROOT/$host/skills/$skill/SKILL.md"
  done
done
for worker in fix-light fix-standard fix-heavy epic-analyst; do
  check_cli_selector "$ROOT/.claude/agents/$worker.md"
  check_cli_selector "$ROOT/.codex/agents/$worker.toml"
done

# An executable wrapper must win even when PATH also provides a CLI.
cat >"$TEST_DIR/bin/putnami" <<'STUB'
#!/usr/bin/env bash
echo "finalize-pr test: PATH CLI used despite the workspace wrapper" >&2
exit 90
STUB
chmod +x "$TEST_DIR/bin/putnami"
# The finalizer never calls a backend client directly.
cat >"$TEST_DIR/bin/gh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${TEST_STATE_DIR:?}/gh-invocations"
echo "finalize-pr test: the finalizer called a backend client directly: gh $*" >&2
exit 91
STUB
chmod +x "$TEST_DIR/bin/gh"

REAL_GIT_BIN="$(command -v git)"
cat >"$TEST_DIR/bin/git" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  add|commit|push)
    printf '%s\n' "$*" >>"${TEST_STATE_DIR:?}/git-mutations"
    ;;
esac
if [ -n "${TEST_STATE_DIR:-}" ] && [ -f "$TEST_STATE_DIR/fail-staged-inspection" ] &&
  [ "${1:-}" = "diff" ] && [ "${2:-}" = "--cached" ] && [ "${3:-}" = "--name-only" ]; then
  exit 73
fi
exec "${REAL_GIT_BIN:?}" "$@"
STUB
chmod +x "$TEST_DIR/bin/git"

git init --bare "$TEST_DIR/origin.git" >/dev/null
git init -b main "$TEST_DIR/work" >/dev/null
git -C "$TEST_DIR/work" config user.name "Skill Test"
git -C "$TEST_DIR/work" config user.email "skill-test@example.com"
echo "initial" >"$TEST_DIR/work/example.txt"
cat >"$TEST_DIR/work/putnamiw" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
# Collaboration calls reach the real CLI and the local provider it routes to.
# Failures are injected here, at the CLI boundary. An unresolved upsert is the
# envelope the CLI prints when the provider times out after the request left;
# its reconcile hint is the one the CLI writes, which the Go harness passes in
# PUTNAMI_TEST_UPSERT_RECONCILE from the CLI's own hint producer.
case "${1:-}" in
  tree)
    [ "${2:-}" = verify ]
    shift 2
    # no-tree-verify stands for a CLI older than `tree verify`, which answers
    # the way the published one does.
    if [ -f "$TEST_STATE_DIR/no-tree-verify" ]; then
      printf 'putnami: unknown subcommand: tree verify\n  Available: fingerprint\n' >&2
      exit 2
    fi
    # The CLI's own tests own what the verifier accepts; this double answers as
    # told and logs what it was asked. Every refusal is a JSON verdict on
    # stdout, a usage error included.
    if [ "$#" -eq 0 ]; then
      printf '{"verdict":"not-verified","reason":"usage: putnami tree verify (--record FILE | --snapshot | --ref FILE | --gate SESSION --report REPORT) [--base REV]"}\n'
      exit 1
    fi
    if [ "${1:-}" = --gate ]; then
      [ "$#" -eq 6 ] && [ "$3" = --report ] && [ "$5" = --base ] && [ "$6" = origin/main ]
      printf '%s %s\n' "$2" "$4" >>"$TEST_STATE_DIR/gate-reuse-invocations"
      # reject-gate-reuse lists the session paths to refuse, or `*` for every one.
      if [ -f "$TEST_STATE_DIR/reject-gate-reuse" ] &&
        grep -Fxq -e '*' -e "$2" "$TEST_STATE_DIR/reject-gate-reuse"; then
        printf '{"verdict":"not-verified","reason":"gate is narrower than the native unfiltered impacted plan"}\n'
        exit 1
      fi
      printf '{"verdict":"gate reusable"}\n'
      exit 0
    fi
    [ "$#" -eq 4 ] && [ "$1" = --record ] && [ "$3" = --base ]
    [ "$2" = "${EXPECTED_RECORD:?}" ]
    # The local main is stale here (the upstream case above resets it), so the
    # checker must receive the remote base the proposal targets, not the local branch.
    [ "$4" = "${EXPECTED_VERIFY_BASE:-origin/main}" ]
    printf '%s\n' "$2" >>"$TEST_STATE_DIR/verification-invocations"
    ordinal="$(wc -l <"$TEST_STATE_DIR/verification-invocations" | tr -d '[:space:]')"
    case "$(cat "$2")" in
      base-mismatch) echo "verification: dossier base does not cover the intended proposal base" >&2; exit 85 ;;
      reject) exit 82 ;;
      reject-second) [ "$ordinal" -lt 2 ] || exit 83 ;;
      mutate) printf 'mutation during verification\n' >>example.txt ;;
      accept) ;;
      *) exit 84 ;;
    esac
    printf '{"verified":true}\n'
    exit 0
    ;;
  tasks|proposals|memory)
    printf '%s\n' "$*" >>"${TEST_STATE_DIR:?}/collab-invocations"
    if [ "${1:-} ${2:-}" = "proposals upsert" ] && [ -f "$TEST_STATE_DIR/unresolved-upsert" ]; then
      jq -cn --arg reconcile "${PUTNAMI_TEST_UPSERT_RECONCILE:?}" '{
        contract: "proposals", version: 1, operation: "upsert",
        provider: {name: "@putnami/local-collaboration"}, outcome: "unresolved",
        error: {
          message: "extension tool local-collaboration.proposals.upsert timed out after 30s; the proposals.upsert write may have happened",
          reason: "provider.timeout", retryable: false, reconcile: $reconcile
        }
      }'
      exit 1
    fi
    # A provider that does not report assignees or labels on its proposals.
    if [ -f "$TEST_STATE_DIR/unreported-members" ] &&
      { [ "${1:-} ${2:-}" = "proposals upsert" ] || [ "${1:-} ${2:-}" = "proposals status" ]; }; then
      "${PUTNAMI_TEST_CLI:?}" "$@" | jq -c 'del(.result.proposal.assignees, .result.proposal.labels)'
      exit "${PIPESTATUS[0]}"
    fi
    # A provider whose upsert answers with a label it applied from its own
    # settings, which the proposal read back does not carry.
    if [ "${1:-} ${2:-}" = "proposals upsert" ] && [ -f "$TEST_STATE_DIR/upsert-reports-label" ]; then
      "${PUTNAMI_TEST_CLI:?}" "$@" | jq -c '.result.proposal.labels += ["area/tooling"]'
      exit "${PIPESTATUS[0]}"
    fi
    # A tasks provider that does not offer transition, as its capabilities say.
    if [ "${1:-} ${2:-}" = "tasks capabilities" ] && [ -f "$TEST_STATE_DIR/transition-unsupported" ]; then
      "${PUTNAMI_TEST_CLI:?}" "$@" | jq -c '.result.operations |= map(if .name == "transition" then .supported = false else . end)'
      exit "${PIPESTATUS[0]}"
    fi
    # The verification comment's request, kept for the assertions below.
    if [ "${1:-} ${2:-}" = "proposals review" ]; then
      request="$(cat)"
      printf '%s\n' "$request" >>"$TEST_STATE_DIR/review-requests"
      printf '%s' "$request" | "${PUTNAMI_TEST_CLI:?}" "$@"
      exit "$?"
    fi
    # A provider with hosted checks: each status read answers with the next
    # line of hosted-checks, and the last line keeps answering.
    if [ "${1:-} ${2:-}" = "proposals status" ] && [ -f "$TEST_STATE_DIR/hosted-checks" ]; then
      checks="$(head -1 "$TEST_STATE_DIR/hosted-checks")"
      if [ "$(wc -l <"$TEST_STATE_DIR/hosted-checks" | tr -d '[:space:]')" -gt 1 ]; then
        tail -n +2 "$TEST_STATE_DIR/hosted-checks" >"$TEST_STATE_DIR/hosted-checks.next"
        mv "$TEST_STATE_DIR/hosted-checks.next" "$TEST_STATE_DIR/hosted-checks"
      fi
      "${PUTNAMI_TEST_CLI:?}" "$@" | jq -c --argjson checks "$checks" '.result.checks = $checks'
      exit "${PIPESTATUS[0]}"
    fi
    exec "${PUTNAMI_TEST_CLI:?}" "$@"
    ;;
esac

# One line per gate invocation, logged before any failure injection so the
# count below sees every invocation the finalizer made, not only the ones
# that succeeded.
printf '%s\n' "$*" >>"${TEST_STATE_DIR:?}/gate-invocations"

# How this invocation ends. `fail-gate` reddens every run — the genuinely broken
# tree, which no retry may rescue. `fail-gate-once` reddens only the first — the
# transient red that a later run replays from its cache entry, and that
# --retry-failed clears by re-executing the task.
gate_outcome="success"
gate_exit=0
if [ -f "$TEST_STATE_DIR/fail-gate" ]; then
  gate_outcome="failed"
  gate_exit=74
elif [ -f "$TEST_STATE_DIR/fail-gate-once" ]; then
  rm -f "$TEST_STATE_DIR/fail-gate-once"
  gate_outcome="failed"
  gate_exit=74
fi

# The real CLI records a session and stamps the tree it opened on BEFORE its
# first task runs — a red run records one too — so the stub does the same, in
# that order: the mutation below has to land after the record for the
# finalizer's comparison to mean what it means in production.
if [ ! -f "$TEST_STATE_DIR/norecord-gate" ]; then
  # The suffix is a zero-padded counter on its own never-reset file: session
  # ids sort lexicographically, and it has to keep rising across the whole test.
  ordinal=$(($(cat "$TEST_STATE_DIR/session-ordinal" 2>/dev/null || echo 0) + 1))
  printf '%s\n' "$ordinal" >"$TEST_STATE_DIR/session-ordinal"
  session=".putnami/sessions/$(date -u +%Y%m%d-%H%M%S)-$(printf '%06d' "$ordinal")"
  mkdir -p "$session"
  cat >"$session/session.json" <<RECORD
{
  "protocolVersion": 2,
  "sessionId": "$(basename "$session")",
  "commands": ["lint", "test", "build", "validate"],
  "tree": { "fingerprint": "$(bash "${STUB_TREE_FINGERPRINT:?}")", "dirty": true, "headSHA": "$(git rev-parse HEAD)" },
  "run": { "outcome": "$gate_outcome", "exitCode": $gate_exit }
}
RECORD
fi

if [ "$gate_exit" -ne 0 ]; then
  # A red gate names the task it failed on, and the finalizer must let that
  # reach its caller rather than swallow it behind the retry.
  printf '%s\n' "test/other lint~lint FAIL" >&2
  exit "$gate_exit"
fi

[ ! -f "$TEST_STATE_DIR/mutate-gate" ] || printf 'gate-mutated\n' >example.txt
printf 'gate ok: %s\n' "$*"
STUB
chmod +x "$TEST_DIR/work/putnamiw"
# The workspace binds both contracts to the local provider and sets the policy
# members the finalizer reads: the delivered state, the task reference line and
# the English language rule.
cat >"$TEST_DIR/work/putnami.workspace.json" <<JSON
{
  "name": "finalize-test",
  "extensions": ["$PUTNAMI_TEST_LOCAL_PROVIDER"],
  "options": {
    "collaboration": {
      "tasks": {"provider": "@putnami/local-collaboration", "version": 1},
      "proposals": {"provider": "@putnami/local-collaboration", "version": 1, "settings": {"repository": "example/work"}}
    },
    "@putnami/contributor": {
      "version": 1,
      "states": {"delivered": "blocked"},
      "publication": {"taskReference": "Closes #{id}"},
      "verification": {"language": "en"}
    }
  }
}
JSON
# A real workspace ignores its session and collaboration stores, and the
# fingerprint honours that: without this, the record the gate writes would
# itself become an untracked file and the post-gate tree would differ from the
# pre-gate one on every run.
printf '.putnami/\n' >"$TEST_DIR/work/.gitignore"
git -C "$TEST_DIR/work" add example.txt putnamiw .gitignore putnami.workspace.json
git -C "$TEST_DIR/work" commit -m "initial" >/dev/null
git -C "$TEST_DIR/work" remote add origin "$TEST_DIR/origin.git"
git -C "$TEST_DIR/work" push -u origin main >/dev/null
git -C "$TEST_DIR/work" checkout -b fix/portable >/dev/null

# A real session store holds records that are NOT gates — a gate spawns nested
# sessions of its own, and every other command records one too. This one sorts
# LAST on purpose: the selector has to skip it and still find the gate.
mkdir -p "$TEST_DIR/work/.putnami/sessions/29991231-235959-000000"
cat >"$TEST_DIR/work/.putnami/sessions/29991231-235959-000000/session.json" <<'RECORD'
{
  "protocolVersion": 2,
  "sessionId": "29991231-235959-000000",
  "commands": ["build"],
  "run": { "outcome": "success", "exitCode": 0 }
}
RECORD

export TEST_STATE_DIR="$TEST_DIR/state"
export REAL_GIT_BIN
# The stub CLI stamps its session record with the real fingerprint, the same way
# the real CLI does: a stub that invented a digest would let the finalizer's
# comparison pass on a tree nobody measured.
export STUB_TREE_FINGERPRINT="$TEST_DIR/shipped/skills/fix/scripts/tree-fingerprint.sh"
export PATH="$TEST_DIR/bin:$PATH"

# collab <contract> <operation> <request>: a read or setup call through the
# workspace's own CLI, printing the result document.
collab() {
  (cd "$TEST_DIR/work" && ./putnamiw "$1" "$2" --output=json --input "$3") | jq -ec '.result'
}
TASK="$(collab tasks create '{"title":"Portable finalizer","idempotencyKey":"finalize-test:task"}' | jq -c '.task.ref')"
TASK_SOURCE="$(jq -r '.source' <<<"$TASK")"
TASK_ID="$(jq -r '.id' <<<"$TASK")"
task_state() {
  collab tasks get "$(jq -cn --argjson ref "$TASK" '{ref: $ref}')" | jq -r '.task.state'
}
open_proposals() {
  collab proposals find '{"change":{"base":"main","head":"fix/portable"},"states":["draft","open"]}' | jq '.items | length'
}
assert_no_proposal() {
  if grep -q '^proposals upsert' "$TEST_DIR/state/collab-invocations" 2>/dev/null; then
    echo "finalize-pr test: a proposal was published" >&2
    exit 1
  fi
}
[ "$(task_state)" = open ]

echo "fixed" >"$TEST_DIR/work/example.txt"
cat >"$TEST_DIR/body.md" <<'BODY'
The finalizer publishes this change through the contracts.

Deletes: nothing.
BODY

# finalize [extra arguments...]: the finalizer with the common arguments.
finalize() {
  (
    cd "$TEST_DIR/work"
    bash "${FINALIZER_UNDER_TEST:-$FINALIZER}" \
      --task-source "$TASK_SOURCE" \
      --task-id "$TASK_ID" \
      --base main \
      --branch fix/portable \
      "$@"
  )
}

if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/missing-proof-status.out" 2>"$TEST_DIR/missing-proof-status.err"; then
  echo "finalize-pr test: expected missing proof status failure" >&2
  exit 1
fi
grep -Fq -- "--proof-status is required" "$TEST_DIR/missing-proof-status.err"

if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status blocked --proof "composition unavailable" \
  --project test/project --file example.txt \
  >"$TEST_DIR/invalid-proof-status.out" 2>"$TEST_DIR/invalid-proof-status.err"; then
  echo "finalize-pr test: expected invalid proof status failure" >&2
  exit 1
fi
grep -Fq -- "--proof-status must be passed or not-applicable" "$TEST_DIR/invalid-proof-status.err"

if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "   " \
  --project test/project --file example.txt \
  >"$TEST_DIR/blank-proof.out" 2>"$TEST_DIR/blank-proof.err"; then
  echo "finalize-pr test: expected blank proof failure" >&2
  exit 1
fi
grep -Fq -- "--proof must contain evidence" "$TEST_DIR/blank-proof.err"

# No commit names an agent, so the flag that wrote the trailer is refused.
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --co-author "Test Model <noreply@example.com>" --project test/project --file example.txt \
  >"$TEST_DIR/co-author.out" 2>"$TEST_DIR/co-author.err"; then
  echo "finalize-pr test: expected --co-author to be refused" >&2
  exit 1
fi
grep -Fq -- "--co-author is gone" "$TEST_DIR/co-author.err"

# The provider-specific issue number is gone: a caller names a task reference.
if finalize --issue 42 --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/issue-flag.out" 2>"$TEST_DIR/issue-flag.err"; then
  echo "finalize-pr test: expected --issue to be refused" >&2
  exit 1
fi
grep -Fq -- "--issue is replaced by --task-source and --task-id" "$TEST_DIR/issue-flag.err"
if (
  cd "$TEST_DIR/work"
  bash "$FINALIZER" --task-id "$TASK_ID" --base main --branch fix/portable \
    --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
    --proof-status passed --proof "real harness observed the expected behavior" \
    --project test/project --file example.txt
) >"$TEST_DIR/half-reference.out" 2>"$TEST_DIR/half-reference.err"; then
  echo "finalize-pr test: expected a half task reference to be refused" >&2
  exit 1
fi
grep -Fq -- "pass both or neither" "$TEST_DIR/half-reference.err"

cat >"$TEST_DIR/french-body.md" <<'BODY'
Il faut garder les tableaux, donc nous changeons le parseur.
BODY
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/french-body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/french-body.out" 2>"$TEST_DIR/french-body.err"; then
  echo "finalize-pr test: expected non-English body failure" >&2
  exit 1
fi
grep -Fq "proposal title, body or proof is not in English" "$TEST_DIR/french-body.err"
grep -Fq "proposal title, body and proof: non-English signals: donc, il faut, nous" "$TEST_DIR/french-body.err"
if grep -Fq "gate ok" "$TEST_DIR/french-body.out"; then
  echo "finalize-pr test: the gate ran for a non-English proposal" >&2
  exit 1
fi
assert_no_proposal

if finalize --title "fix(test): préserver les ordres sémantiques" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/french-title.out" 2>"$TEST_DIR/french-title.err"; then
  echo "finalize-pr test: expected non-English title failure" >&2
  exit 1
fi
grep -Fq "proposal title, body or proof is not in English" "$TEST_DIR/french-title.err"
assert_no_proposal

# The proof goes into the verification comment, so it is checked too.
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "le harnais corrige les tableaux dans le parseur" \
  --project test/project --file example.txt \
  >"$TEST_DIR/french-proof.out" 2>"$TEST_DIR/french-proof.err"; then
  echo "finalize-pr test: expected non-English proof failure" >&2
  exit 1
fi
grep -Fq "proposal title, body or proof is not in English" "$TEST_DIR/french-proof.err"
assert_no_proposal

# A change that touches only tests is titled test, whatever skill produced it.
for only_tests in "fix(cli): wait for the attempt record" "feat!: cover the parser"; do
  if finalize --title "$only_tests" --body-file "$TEST_DIR/body.md" \
    --proof-status passed --proof "real harness observed the expected behavior" \
    --project test/project \
    --file internal/run/restart_test.go --file web/src/parser.test.ts --file testdata/case.json \
    >"$TEST_DIR/test-only.out" 2>"$TEST_DIR/test-only.err"; then
    echo "finalize-pr test: expected a test-only change titled '$only_tests' to be refused" >&2
    exit 1
  fi
  grep -Fq "every changed path is a test, so the title type is test" "$TEST_DIR/test-only.err"
  if grep -Fq "gate ok" "$TEST_DIR/test-only.out"; then
    echo "finalize-pr test: the gate ran for a test-only change titled '$only_tests'" >&2
    exit 1
  fi
done
grep -Fq 'retitle it "test!: cover the parser"' "$TEST_DIR/test-only.err"
assert_no_proposal

# The title is the squash commit title, so the type follows every path of the
# branch, committed or enumerated. Run on another branch, each call that passes
# the title check stops at the branch check that follows it, before any
# mutation.
# fix_title_check <name> [argument...]: the finalizer with a fix title, which
# must stop; its output lands in <name>.out and <name>.err.
fix_title_check() {
  local name="$1"
  shift
  if finalize --title "fix(cli): wait for the attempt record" --body-file "$TEST_DIR/body.md" \
    --proof-status passed --proof "real harness observed the expected behavior" \
    --project test/project "$@" >"$TEST_DIR/$name.out" 2>"$TEST_DIR/$name.err"; then
    echo "finalize-pr test: expected the $name finalization to stop" >&2
    exit 1
  fi
}
# passes_title_check <name>: the call stopped at the branch check, not the title.
passes_title_check() {
  if grep -Fq "every changed path is a test" "$TEST_DIR/$1.err"; then
    echo "finalize-pr test: the $1 branch changes product code and was refused a fix title" >&2
    exit 1
  fi
  grep -Fq "current branch 'fix/title-scope' is not expected branch 'fix/portable'" "$TEST_DIR/$1.err"
}
git -C "$TEST_DIR/work" checkout -q -b fix/title-scope
mkdir -p "$TEST_DIR/work/internal/run"
# A non-ASCII name stays unquoted, so the test pattern still reads it as a test.
printf 'package run\n' >"$TEST_DIR/work/internal/run/naïve_test.go"
git -C "$TEST_DIR/work" add internal/run
git -C "$TEST_DIR/work" commit -q -m "test(cli): cover the attempt record"
printf 'package run\n' >"$TEST_DIR/work/internal/run/restart_test.go"
fix_title_check test-branch --file internal/run/restart_test.go
grep -Fq "every changed path is a test, so the title type is test" "$TEST_DIR/test-branch.err"
# An enumerated product file changes the product, whatever the commits hold.
printf 'package run\n' >"$TEST_DIR/work/internal/run/restart.go"
fix_title_check product-file --file internal/run/restart.go
passes_title_check product-file
# A committed product file does too, when the enumerated files are all tests.
printf 'package run\n' >"$TEST_DIR/work/internal/run/attempt.go"
git -C "$TEST_DIR/work" add internal/run/attempt.go
git -C "$TEST_DIR/work" commit -q -m "fix(cli): wait for the attempt record"
fix_title_check product-branch --file internal/run/restart_test.go
passes_title_check product-branch
# A product file renamed into a test path deletes product code.
git -C "$TEST_DIR/work" rm -q internal/run/attempt.go
mkdir "$TEST_DIR/work/testdata"
git -C "$TEST_DIR/work" mv .gitignore testdata/.gitignore
git -C "$TEST_DIR/work" commit -q -m "fix(cli): move the ignore file"
fix_title_check renamed-product --file internal/run/restart_test.go
passes_title_check renamed-product
# A base that does not resolve leaves the branch's paths unknown: the
# finalizer refuses rather than judging the enumerated files alone.
fix_title_check missing-base --base no-such-base --file internal/run/restart_test.go
grep -Fq "cannot list the paths of the branch against 'no-such-base'" "$TEST_DIR/missing-base.err"
git -C "$TEST_DIR/work" checkout -q fix/portable
rm -r "$TEST_DIR/work/internal"
git -C "$TEST_DIR/work" branch -q -D fix/title-scope
assert_no_proposal

# The body is the squash commit message: no heading, no agent trailer or
# "Generated with" line, and no longer than the policy allows. Each is refused
# before the gate.
printf '## Summary\n\nThe finalizer keeps the body plain.\n' >"$TEST_DIR/heading-body.md"
printf 'The finalizer keeps the body plain.\n\nCo-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>\n' >"$TEST_DIR/trailer-body.md"
printf 'The finalizer keeps the body plain.\n\n\xf0\x9f\xa4\x96 Generated with [Claude Code](https://claude.com/claude-code)\n' >"$TEST_DIR/generated-body.md"
for case_body in heading:"carries no markdown heading" trailer:"names an agent" generated:"names an agent"; do
  name="${case_body%%:*}"
  if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/$name-body.md" \
    --proof-status passed --proof "real harness observed the expected behavior" \
    --project test/project --file example.txt \
    >"$TEST_DIR/$name-body.out" 2>"$TEST_DIR/$name-body.err"; then
    echo "finalize-pr test: expected the $name body to be refused" >&2
    exit 1
  fi
  grep -Fq "${case_body#*:}" "$TEST_DIR/$name-body.err" ||
    { echo "finalize-pr test: the $name body was refused for another reason" >&2; cat "$TEST_DIR/$name-body.err" >&2; exit 1; }
done
# A human co-author is not an agent.
printf 'The finalizer keeps the body plain.\n\nCo-Authored-By: Ada Lovelace <ada@example.com>\n' >"$TEST_DIR/human-body.md"
cp "$TEST_DIR/work/putnami.workspace.json" "$TEST_DIR/workspace.json.plain"
jq '.options["@putnami/contributor"].publication.bodyMaxBytes = 40' "$TEST_DIR/workspace.json.plain" >"$TEST_DIR/work/putnami.workspace.json"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/human-body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/long-body.out" 2>"$TEST_DIR/long-body.err"; then
  echo "finalize-pr test: expected a body over publication.bodyMaxBytes to be refused" >&2
  exit 1
fi
grep -Fq "and the policy allows 40 (publication.bodyMaxBytes)" "$TEST_DIR/long-body.err"
cp "$TEST_DIR/workspace.json.plain" "$TEST_DIR/work/putnami.workspace.json"
if grep -Fq "gate ok" "$TEST_DIR"/*-body.out; then
  echo "finalize-pr test: the gate ran for a refused body" >&2
  exit 1
fi
assert_no_proposal

# Without the detector beside it, the finalizer refuses instead of skipping the
# language rule the policy sets.
mkdir -p "$TEST_DIR/lone/skills/fix/scripts"
cp "$FINALIZER" "$TEST_DIR/lone/skills/fix/scripts/finalize-pr.sh"
if FINALIZER_UNDER_TEST="$TEST_DIR/lone/skills/fix/scripts/finalize-pr.sh" finalize \
  --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/lone.out" 2>"$TEST_DIR/lone.err"; then
  echo "finalize-pr test: expected missing detector failure" >&2
  exit 1
fi
grep -Fq "English-only detector not found" "$TEST_DIR/lone.err"
assert_no_proposal

# Same rule for the fingerprint helper: the finalizer compares the tree before
# and after the gate, and execute compares that same digest across agents. Left
# without the one script that computes it, it refuses rather than skipping.
mkdir -p "$TEST_DIR/nofingerprint/skills/fix/scripts" "$TEST_DIR/nofingerprint/skills/check/scripts"
cp "$FINALIZER" "$TEST_DIR/nofingerprint/skills/fix/scripts/finalize-pr.sh"
cp "$ROOT/.agents/skills/check/scripts/english-only.sh" "$TEST_DIR/nofingerprint/skills/check/scripts/english-only.sh"
if FINALIZER_UNDER_TEST="$TEST_DIR/nofingerprint/skills/fix/scripts/finalize-pr.sh" finalize \
  --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/nofingerprint.out" 2>"$TEST_DIR/nofingerprint.err"; then
  echo "finalize-pr test: expected missing fingerprint helper failure" >&2
  exit 1
fi
grep -Fq "tree fingerprint helper not found" "$TEST_DIR/nofingerprint.err"
assert_no_proposal

# A task the provider does not know is refused before anything is written.
pushed_before="$(git --git-dir="$TEST_DIR/origin.git" rev-parse -q --verify refs/heads/fix/portable || true)"
if (
  cd "$TEST_DIR/work"
  bash "$FINALIZER" --task-source "$TASK_SOURCE" --task-id "no-such-task" --base main --branch fix/portable \
    --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
    --proof-status passed --proof "real harness observed the expected behavior" \
    --project test/project --file example.txt
) >"$TEST_DIR/unknown-task.out" 2>"$TEST_DIR/unknown-task.err"; then
  echo "finalize-pr test: expected an unknown task to be refused" >&2
  exit 1
fi
grep -Fq "reading task no-such-task failed: not_found" "$TEST_DIR/unknown-task.err"
[ "$(git --git-dir="$TEST_DIR/origin.git" rev-parse -q --verify refs/heads/fix/portable || true)" = "$pushed_before" ]
[ ! -s "$TEST_DIR/state/gate-invocations" ]
[ ! -s "$TEST_DIR/state/git-mutations" ]
assert_no_proposal

# A workspace that binds no proposals provider stops before the gate and before
# any mutation, naming the binding to add.
cp "$TEST_DIR/work/putnami.workspace.json" "$TEST_DIR/workspace.json.bound"
jq 'del(.options.collaboration.proposals)' "$TEST_DIR/workspace.json.bound" >"$TEST_DIR/work/putnami.workspace.json"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/unbound.out" 2>"$TEST_DIR/unbound.err"; then
  echo "finalize-pr test: expected an unbound proposals contract to be refused" >&2
  exit 1
fi
grep -Fq "the proposals contract is unbound in this workspace" "$TEST_DIR/unbound.err"
[ ! -s "$TEST_DIR/state/gate-invocations" ]
[ ! -s "$TEST_DIR/state/git-mutations" ]
cp "$TEST_DIR/workspace.json.bound" "$TEST_DIR/work/putnami.workspace.json"
[ -z "$(git -C "$TEST_DIR/work" status --porcelain -- putnami.workspace.json)" ]

# A tasks provider that does not offer transition is refused before the gate
# and before any mutation: the helper would otherwise publish a proposal and
# then fail to deliver its task.
touch "$TEST_DIR/state/transition-unsupported"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/no-transition.out" 2>"$TEST_DIR/no-transition.err"; then
  echo "finalize-pr test: expected a tasks provider without transition to be refused" >&2
  exit 1
fi
rm "$TEST_DIR/state/transition-unsupported"
grep -Fq "the bound tasks provider does not offer transition, which this helper calls; nothing was written" "$TEST_DIR/no-transition.err"
[ ! -s "$TEST_DIR/state/gate-invocations" ]
[ ! -s "$TEST_DIR/state/git-mutations" ]
assert_no_proposal

# A genuinely broken tree: the retry cannot rescue it, and both runs name the
# task that failed, so the caller reads a diagnosis rather than a bare exit code.
touch "$TEST_DIR/state/fail-gate"
rm -f "$TEST_DIR/state/gate-invocations"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/gate-failure.out" 2>"$TEST_DIR/gate-failure.err"; then
  echo "finalize-pr test: expected gate failure" >&2
  exit 1
fi
assert_no_proposal
grep -Fq "test/other lint~lint FAIL" "$TEST_DIR/gate-failure.err"
if [ "$(wc -l <"$TEST_DIR/state/gate-invocations" | tr -d '[:space:]')" != "2" ]; then
  echo "finalize-pr test: a red gate must be retried exactly once:" >&2
  cat "$TEST_DIR/state/gate-invocations" >&2
  exit 1
fi
grep -Fxq "lint,test,build,validate --impacted --enforce-coverage --retry-failed" "$TEST_DIR/state/gate-invocations"
rm "$TEST_DIR/state/fail-gate"

touch "$TEST_DIR/state/mutate-gate"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/gate-mutation.out" 2>"$TEST_DIR/gate-mutation.err"; then
  echo "finalize-pr test: expected gate mutation failure" >&2
  exit 1
fi
grep -Fq "gate changed the working tree; rerun local proof" "$TEST_DIR/gate-mutation.err"
assert_no_proposal
rm "$TEST_DIR/state/mutate-gate"
printf 'fixed\n' >"$TEST_DIR/work/example.txt"

# The pre-gate tree comes from the record the gate wrote. A gate that recorded
# no session leaves nothing to compare against, and the finalizer must say so
# rather than fall back to a digest of its own.
touch "$TEST_DIR/state/norecord-gate"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project --file example.txt \
  >"$TEST_DIR/gate-norecord.out" 2>"$TEST_DIR/gate-norecord.err"; then
  echo "finalize-pr test: expected a failure when the gate recorded no session" >&2
  exit 1
fi
grep -Fq "the gate recorded no session" "$TEST_DIR/gate-norecord.err"
assert_no_proposal
rm "$TEST_DIR/state/norecord-gate"
rm -f "$TEST_DIR/state/gate-invocations" "$TEST_DIR/state/collab-invocations"

# Two projects, so a per-project replay would be visible as a second gate line.
first_output="$(finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  \
  --project test/project --project test/other --file example.txt 2>"$TEST_DIR/first.err")"
PROPOSAL="$(sed -n 's/^PROPOSAL_REF=//p' <<<"$first_output")"
[ -n "$PROPOSAL" ] || { echo "finalize-pr test: no PROPOSAL_REF line:" >&2; echo "$first_output" >&2; exit 1; }
# The local provider has no hosted page and no hosted checks, and says so.
if grep -q '^PROPOSAL_URL=' <<<"$first_output"; then
  echo "finalize-pr test: a URL was printed for a provider that has none" >&2
  exit 1
fi
grep -Fq "proposal checks: unsupported" "$TEST_DIR/first.err"
# The local provider reports assignees and labels, and a local proposal has
# none: the helper verified both lists and prints them.
for member in assignees labels; do
  grep -Fxq "finalize-pr: proposal $member: none" "$TEST_DIR/first.err" ||
    { echo "finalize-pr test: the helper did not verify the proposal's $member" >&2; cat "$TEST_DIR/first.err" >&2; exit 1; }
done
grep -Fq "task $TASK_ID: blocked (blocked)" "$TEST_DIR/first.err"
# Every request went to the CLI on standard input: none is on a command line,
# where an operating system limits one argument's size.
if grep -Eq -- '--input( |=)' "$TEST_DIR/state/collab-invocations"; then
  echo "finalize-pr test: a request was passed on the command line:" >&2
  cat "$TEST_DIR/state/collab-invocations" >&2
  exit 1
fi
grep -Fxq "proposals upsert --input-file - --output=json" "$TEST_DIR/state/collab-invocations"
[ -z "$(git -C "$TEST_DIR/work" status --porcelain)" ]

# One gate session per finalize, whatever --project names: the implementer
# already gated the touched projects on this tree, so the finalizer adds only
# the impacted run that reaches the change's dependents.
gate_invocations="$(wc -l <"$TEST_DIR/state/gate-invocations" | tr -d '[:space:]')"
if [ "$gate_invocations" != "1" ]; then
  echo "finalize-pr test: expected exactly 1 gate invocation, got $gate_invocations:" >&2
  cat "$TEST_DIR/state/gate-invocations" >&2
  exit 1
fi
grep -Fxq "lint,test,build,validate --impacted --enforce-coverage" "$TEST_DIR/state/gate-invocations"
if grep -Fq -- "--projects" "$TEST_DIR/state/gate-invocations"; then
  echo "finalize-pr test: the finalizer replayed a per-project gate" >&2
  exit 1
fi
git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable >/dev/null
# The provider holds exactly what the helper vouched for: base, head, pushed
# commit, the task reference line and the verification record.
collab proposals status "$(jq -cn --argjson ref "$PROPOSAL" '{ref: $ref}')" >"$TEST_DIR/status.json"
[ "$(jq -r '.proposal.change.base' "$TEST_DIR/status.json")" = main ]
[ "$(jq -r '.proposal.change.head' "$TEST_DIR/status.json")" = fix/portable ]
[ "$(jq -r '.proposal.change.headCommit' "$TEST_DIR/status.json")" = "$(git -C "$TEST_DIR/work" rev-parse HEAD)" ]
jq -r '.proposal.body' "$TEST_DIR/status.json" >"$TEST_DIR/published-body.md"
# The body is the squash commit message: the caller's body and the task
# reference line, nothing else. The proposal is ready for review.
[ "$(cat "$TEST_DIR/published-body.md")" = "$(printf '%s\n\nCloses #%s' "$(cat "$TEST_DIR/body.md")" "$TASK_ID")" ]
[ "$(jq -r '.proposal.state' "$TEST_DIR/status.json")" = open ]
# The verification record is a comment on the pushed commit.
[ "$(jq '.reviews | length' "$TEST_DIR/status.json")" = 1 ]
[ "$(jq -r '.reviews[0].verdict' "$TEST_DIR/status.json")" = comment ]
[ "$(jq -r '.reviews[0].commit' "$TEST_DIR/status.json")" = "$(git -C "$TEST_DIR/work" rev-parse HEAD)" ]
last_review() { tail -1 "$TEST_DIR/state/review-requests" | jq -r '.body'; }
last_review >"$TEST_DIR/verification.md"
if grep -Fq -- "example.txt" "$TEST_DIR/verification.md"; then
  echo "finalize-pr test: the verification comment lists files the proposal diff already shows" >&2
  exit 1
fi
grep -Fq -- "pass for test/project test/other, then once with --impacted" "$TEST_DIR/verification.md"
grep -Fq -- "local proof (passed): real harness observed the expected behavior" "$TEST_DIR/verification.md"
# The task moved to the policy's delivered state, through the contract.
[ "$(task_state)" = blocked ]
grep -q '^tasks transition' "$TEST_DIR/state/collab-invocations"
[ -z "$(git -C "$TEST_DIR/work" log -1 --format='%(trailers:key=Co-Authored-By,valueonly)' | sed '/^$/d')" ]
git -C "$TEST_DIR/work" log -1 --format=%B | grep -Fxq "Closes #$TASK_ID"

# A resumed call finds the same proposal and the task already in the delivered
# state. It publishes no second proposal, and it still sends the transition:
# a task can read as the delivered state through a provider representation
# the delivery must replace, and only the provider can tell. The local
# provider has one representation per state, so the task does not change.
delivered_revision="$(collab tasks get "$(jq -cn --argjson ref "$TASK" '{ref: $ref}')" | jq -r '.task.revision')"
rm -f "$TEST_DIR/state/collab-invocations"
second_output="$(finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project)"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$second_output")" = "$PROPOSAL" ]
[ "$(open_proposals)" = 1 ]
[ "$(grep -c '^tasks transition' "$TEST_DIR/state/collab-invocations")" = 1 ]
[ "$(collab tasks get "$(jq -cn --argjson ref "$TASK" '{ref: $ref}')" | jq -r '.task.revision')" = "$delivered_revision" ]
[ "$(task_state)" = blocked ]

# A provider that does not report assignees or labels is not verified on them,
# and the helper says so rather than calling the proposal verified on them.
touch "$TEST_DIR/state/unreported-members"
unreported_output="$(finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project 2>"$TEST_DIR/unreported.err")"
rm "$TEST_DIR/state/unreported-members"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$unreported_output")" = "$PROPOSAL" ]
for member in assignees labels; do
  grep -Fxq "finalize-pr: the proposals provider does not report $member; they are not verified" "$TEST_DIR/unreported.err" ||
    { echo "finalize-pr test: unreported $member were not disclosed" >&2; cat "$TEST_DIR/unreported.err" >&2; exit 1; }
done

# A label the provider answered with on the upsert, and that the proposal read
# back does not carry, stops the helper before the task moves and before any
# proposal reference is printed.
collab tasks transition "$(jq -cn --argjson ref "$TASK" '{ref: $ref, state: "in_progress"}')" >/dev/null
rm -f "$TEST_DIR/state/collab-invocations"
touch "$TEST_DIR/state/upsert-reports-label"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project \
  >"$TEST_DIR/dropped-label.out" 2>"$TEST_DIR/dropped-label.err"; then
  echo "finalize-pr test: expected a label missing from the read-back to stop the helper" >&2
  exit 1
fi
rm "$TEST_DIR/state/upsert-reports-label"
grep -Fq "proposal labels verification failed: the upsert answered with area/tooling, and the proposal read back does not carry it" "$TEST_DIR/dropped-label.err" ||
  { echo "finalize-pr test: the dropped label was not named" >&2; cat "$TEST_DIR/dropped-label.err" >&2; exit 1; }
if grep -q '^PROPOSAL_REF=' "$TEST_DIR/dropped-label.out" || grep -q '^tasks transition' "$TEST_DIR/state/collab-invocations"; then
  echo "finalize-pr test: an unverified proposal printed its reference or moved the task" >&2
  exit 1
fi
[ "$(task_state)" = in_progress ]
collab tasks transition "$(jq -cn --argjson ref "$TASK" '{ref: $ref, state: "blocked"}')" >/dev/null

# A proposal body over the contract's 65536 bytes is refused before the push:
# the branch never reaches the remote without its proposal.
printf 'oversized\n' >>"$TEST_DIR/work/example.txt"
git -C "$TEST_DIR/work" commit -q -am "fix(test): oversized body"
pushed_before="$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)"
for _ in $(seq 1 1100); do
  printf '%s\n' "The finalizer keeps every proposal body within the contract limit."
done >"$TEST_DIR/oversized-body.md"
rm -f "$TEST_DIR/state/collab-invocations" "$TEST_DIR/state/git-mutations"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/oversized-body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project \
  >"$TEST_DIR/oversized.out" 2>"$TEST_DIR/oversized.err"; then
  echo "finalize-pr test: expected an oversized body to be refused" >&2
  exit 1
fi
grep -Fq "and the proposals contract carries at most 65536" "$TEST_DIR/oversized.err"
[ "$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)" = "$pushed_before" ]
if grep -q '^push' "$TEST_DIR/state/git-mutations" 2>/dev/null; then
  echo "finalize-pr test: an oversized body was pushed" >&2
  exit 1
fi
assert_no_proposal

# A tree whose gate recorded a transient failure finalizes on its own. Without
# the retry, that verdict is replayed at the same cache key on every later call.
rm -f "$TEST_DIR/state/gate-invocations"
touch "$TEST_DIR/state/fail-gate-once"
transient_output="$(finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project)"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$transient_output")" = "$PROPOSAL" ]
[ ! -f "$TEST_DIR/state/fail-gate-once" ]
if [ "$(wc -l <"$TEST_DIR/state/gate-invocations" | tr -d '[:space:]')" != "2" ]; then
  echo "finalize-pr test: expected the red gate plus one retry:" >&2
  cat "$TEST_DIR/state/gate-invocations" >&2
  exit 1
fi
# The retry is the SECOND run, never the first: re-executing a legitimately
# cached failure on every call would pay for it on the green runs too.
[ "$(head -1 "$TEST_DIR/state/gate-invocations")" = "lint,test,build,validate --impacted --enforce-coverage" ]
[ "$(tail -1 "$TEST_DIR/state/gate-invocations")" = "lint,test,build,validate --impacted --enforce-coverage --retry-failed" ]

touch "$TEST_DIR/state/fail-staged-inspection"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project \
  >"$TEST_DIR/staged-failure.out" 2>"$TEST_DIR/staged-failure.err"; then
  echo "finalize-pr test: expected staged inspection failure" >&2
  exit 1
fi
grep -Fq "failed to inspect pre-staged paths" "$TEST_DIR/staged-failure.err"
rm "$TEST_DIR/state/fail-staged-inspection"

# A resumed branch with a commit the helper did not create is refused before
# the push when that commit names an agent.
printf 'resumed\n' >"$TEST_DIR/work/example.txt"
git -C "$TEST_DIR/work" commit -q -am "fix(test): resumed with an agent trailer" -m "Co-Authored-By: GPT-6 Astra <noreply@openai.com>"
pushed_before="$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project \
  >"$TEST_DIR/agent-trailer.out" 2>"$TEST_DIR/agent-trailer.err"; then
  echo "finalize-pr test: expected a commit naming an agent to be refused" >&2
  exit 1
fi
grep -Fq "names an agent; reword it without that line" "$TEST_DIR/agent-trailer.err"
[ "$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)" = "$pushed_before" ]
git -C "$TEST_DIR/work" commit -q --amend -m "fix(test): resumed without an agent trailer"

# Commits other people landed on the base, reachable through a merge while the
# local base is stale, are not this change's: the check compares against
# origin/<base>, so their messages are not read.
git -C "$TEST_DIR/work" checkout -q main
printf 'upstream\n' >"$TEST_DIR/work/upstream.txt"
git -C "$TEST_DIR/work" add upstream.txt
git -C "$TEST_DIR/work" commit -q -m "feat: land upstream work" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git -C "$TEST_DIR/work" push -q origin main
git -C "$TEST_DIR/work" reset -q --hard HEAD~1
git -C "$TEST_DIR/work" checkout -q fix/portable
git -C "$TEST_DIR/work" merge -q --no-ff --no-edit origin/main
merged_output="$(finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project)"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$merged_output")" = "$PROPOSAL" ]
[ "$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)" = "$(git -C "$TEST_DIR/work" rev-parse HEAD)" ]
# The republished proposal follows the pushed head.
collab proposals status "$(jq -cn --argjson ref "$PROPOSAL" '{ref: $ref}')" >"$TEST_DIR/status.json"
[ "$(jq -r '.proposal.change.headCommit' "$TEST_DIR/status.json")" = "$(git -C "$TEST_DIR/work" rev-parse HEAD)" ]

# An uncertain write stops the helper: it names the read that reconciles it,
# never repeats the write, and never moves the task on an unknown outcome.
collab tasks transition "$(jq -cn --argjson ref "$TASK" '{ref: $ref, state: "in_progress"}')" >/dev/null
rm -f "$TEST_DIR/state/collab-invocations"
touch "$TEST_DIR/state/unresolved-upsert"
if finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project \
  >"$TEST_DIR/unresolved.out" 2>"$TEST_DIR/unresolved.err"; then
  echo "finalize-pr test: expected an unresolved upsert to stop the helper" >&2
  exit 1
fi
rm "$TEST_DIR/state/unresolved-upsert"
grep -Fq "publishing the proposal for fix/portable is unresolved (provider.timeout)" "$TEST_DIR/unresolved.err"
# The CLI's hint is relayed as it is, and it names the reconciling read as
# <contract>.<operation>, the form the contract requires of every hint.
case "$PUTNAMI_TEST_UPSERT_RECONCILE" in
  *proposals.find*) ;;
  *) echo "finalize-pr test: the CLI's upsert hint names no proposals.find: $PUTNAMI_TEST_UPSERT_RECONCILE" >&2; exit 1 ;;
esac
grep -Fq "reconcile with: $PUTNAMI_TEST_UPSERT_RECONCILE, then finalize again" "$TEST_DIR/unresolved.err" ||
  { echo "finalize-pr test: the reconciling read was not relayed" >&2; cat "$TEST_DIR/unresolved.err" >&2; exit 1; }
[ "$(grep -c '^proposals upsert' "$TEST_DIR/state/collab-invocations")" = 1 ]
if grep -q '^tasks transition' "$TEST_DIR/state/collab-invocations"; then
  echo "finalize-pr test: the task moved after an unresolved publication" >&2
  exit 1
fi
[ "$(task_state)" = in_progress ]
if grep -q '^PROPOSAL_REF=' "$TEST_DIR/unresolved.out"; then
  echo "finalize-pr test: an unresolved publication printed a proposal reference" >&2
  exit 1
fi

# Consumer repositories may install only the PATH CLI. Exercise the real
# finalizer and its retry through that entrypoint, with real git, a local
# remote and the real collaboration route.
cp "$TEST_DIR/work/putnamiw" "$TEST_DIR/bin/putnami"
git -C "$TEST_DIR/work" rm -q putnamiw
git -C "$TEST_DIR/work" commit -q -m "fix(test): use the installed CLI"
# collab now reaches the provider through the installed CLI.
collab() {
  (cd "$TEST_DIR/work" && putnami "$1" "$2" --output=json --input "$3") | jq -ec '.result'
}
rm -f "$TEST_DIR/state/gate-invocations"
touch "$TEST_DIR/state/fail-gate-once"
installed_output="$(finalize --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
  --proof-status passed --proof "real harness observed the expected behavior" \
  --project test/project)"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$installed_output")" = "$PROPOSAL" ]
[ "$(wc -l <"$TEST_DIR/state/gate-invocations" | tr -d '[:space:]')" = 2 ]
[ "$(head -1 "$TEST_DIR/state/gate-invocations")" = "lint,test,build,validate --impacted --enforce-coverage" ]
[ "$(tail -1 "$TEST_DIR/state/gate-invocations")" = "lint,test,build,validate --impacted --enforce-coverage --retry-failed" ]
[ "$(task_state)" = blocked ]

# The verification-mode tests isolate the finalizer/checker boundary. The
# checker is the CLI's `tree verify`, whose own tests own dossier semantics; the
# stub CLI's double proves that refusal prevents mutation, accepted evidence
# avoids a repeated gate, and the finalizer rechecks before publishing. All Git
# operations still use a real local remote.
mkdir -p "$TEST_DIR/verified/skills/fix/scripts" "$TEST_DIR/verified/skills/check/scripts"
cp "$FINALIZER" "$TEST_DIR/verified/skills/fix/scripts/finalize-pr.sh"
cp "$ROOT/.agents/skills/fix/scripts/tree-fingerprint.sh" "$TEST_DIR/verified/skills/fix/scripts/tree-fingerprint.sh"
cp "$ROOT/.agents/skills/check/scripts/english-only.sh" "$TEST_DIR/verified/skills/check/scripts/english-only.sh"
export EXPECTED_RECORD="$TEST_DIR/work/.putnami/dossier.json"
run_verified_finalizer() {
  FINALIZER_UNDER_TEST="$TEST_DIR/verified/skills/fix/scripts/finalize-pr.sh" finalize \
    --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
    --proof-status passed --proof "evidence referenced by the dossier" \
    --project test/project \
    --verification-file "$EXPECTED_RECORD"
}
reset_verification_logs() {
  rm -f "$TEST_DIR/state/gate-invocations" "$TEST_DIR/state/collab-invocations" "$TEST_DIR/state/verification-invocations" "$TEST_DIR/state/git-mutations"
}
assert_no_publication() {
  [ ! -s "$TEST_DIR/state/gate-invocations" ]
  if grep -Eq '^(proposals (upsert|review|merge)|tasks (create|update|transition|assign|link|claim))' "$TEST_DIR/state/collab-invocations" 2>/dev/null; then
    echo "finalize-pr test: a refused verification still wrote to a provider" >&2
    exit 1
  fi
  [ ! -s "$TEST_DIR/state/git-mutations" ]
  [ "$(git -C "$TEST_DIR/work" rev-parse HEAD)" = "$verified_head" ]
  [ "$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)" = "$verified_remote" ]
}
verified_head="$(git -C "$TEST_DIR/work" rev-parse HEAD)"
verified_remote="$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)"

reset_verification_logs
printf 'accept\n' >"$EXPECTED_RECORD"
printf 'uncommitted\n' >>"$TEST_DIR/work/example.txt"
if run_verified_finalizer >"$TEST_DIR/verified-dirty.out" 2>"$TEST_DIR/verified-dirty.err"; then
  echo "finalize-pr test: expected dirty verification tree refusal" >&2
  exit 1
fi
grep -Fq 'requires a clean, committed tree' "$TEST_DIR/verified-dirty.err"
[ ! -s "$TEST_DIR/state/verification-invocations" ]
assert_no_publication
git -C "$TEST_DIR/work" restore example.txt

# A CLI older than `tree verify` cannot check the dossier: the finalizer asks
# for an upgrade and changes nothing.
reset_verification_logs
touch "$TEST_DIR/state/no-tree-verify"
if run_verified_finalizer >"$TEST_DIR/verified-old-cli.out" 2>"$TEST_DIR/verified-old-cli.err"; then
  echo "finalize-pr test: expected a refusal from a CLI without tree verify" >&2
  exit 1
fi
grep -Fq 'finalize-pr: --verification-file needs `putnami tree verify`, and putnami does not have it; upgrade the Putnami CLI, then finalize again' "$TEST_DIR/verified-old-cli.err"
[ ! -s "$TEST_DIR/state/verification-invocations" ]
assert_no_publication
rm -f "$TEST_DIR/state/no-tree-verify"

for disposition in reject reject-second base-mismatch mutate; do
  reset_verification_logs
  printf '%s\n' "$disposition" >"$EXPECTED_RECORD"
  if run_verified_finalizer >"$TEST_DIR/verified-$disposition.out" 2>"$TEST_DIR/verified-$disposition.err"; then
    echo "finalize-pr test: expected verification refusal: $disposition" >&2
    exit 1
  fi
  assert_no_publication
  if [ "$disposition" = reject-second ]; then
    [ "$(wc -l <"$TEST_DIR/state/verification-invocations" | tr -d '[:space:]')" = 2 ]
  elif [ "$disposition" = base-mismatch ]; then
    grep -Fq 'dossier base does not cover the intended proposal base' "$TEST_DIR/verified-base-mismatch.err"
    [ "$(wc -l <"$TEST_DIR/state/verification-invocations" | tr -d '[:space:]')" = 1 ]
  elif [ "$disposition" = mutate ]; then
    grep -Fq 'tree changed during verification' "$TEST_DIR/verified-mutate.err"
    git -C "$TEST_DIR/work" restore example.txt
  fi
done

reset_verification_logs
printf 'accept\n' >"$EXPECTED_RECORD"
verified_output="$(run_verified_finalizer)"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$verified_output")" = "$PROPOSAL" ]
[ ! -s "$TEST_DIR/state/gate-invocations" ]
[ "$(wc -l <"$TEST_DIR/state/verification-invocations" | tr -d '[:space:]')" = 3 ]
[ "$(git -C "$TEST_DIR/work" rev-parse HEAD)" = "$verified_head" ]
[ -z "$(git -C "$TEST_DIR/work" status --porcelain)" ]
last_review | grep -Fq "reused current evidence accepted by the execute verifier; dossier: $EXPECTED_RECORD"
[ "$(open_proposals)" = 1 ]

# Without a dossier, a green impacted gate that already ran on this exact tree
# is the finalizer's gate: the coordinator ran it, so the finalizer runs none.
# Records that are not candidates never reach the checker: a nested session, a
# red run, a per-project run, a run on another tree and a run without a report.
# The synthetic ids are dated in the past, so every record the stub gate writes
# sorts after them and the finalizer's own-gate bookkeeping still reads it.
reset_verification_logs
rm -f "$TEST_DIR/state/gate-reuse-invocations"
write_gate_record() {
  local id="$1" mode="$2" outcome="$3" tree="$4" parent="${5:-}"
  mkdir -p "$TEST_DIR/work/.putnami/sessions/$id" "$TEST_DIR/work/.putnami/reports"
  jq -n --arg id "$id" --arg mode "$mode" --arg outcome "$outcome" --arg tree "$tree" --arg parent "$parent" '{
    protocolVersion: 2, sessionId: $id, commands: ["lint", "test", "build", "validate"],
    selection: {mode: $mode, scoped: true}, tree: {fingerprint: $tree},
    run: {outcome: $outcome, exitCode: (if $outcome == "success" then 0 else 1 end)}
  } + (if $parent == "" then {} else {parentSessionId: $parent} end)' \
    >"$TEST_DIR/work/.putnami/sessions/$id/session.json"
  printf '{"protocolVersion":2,"sessionId":"%s"}\n' "$id" >"$TEST_DIR/work/.putnami/reports/$id.json"
}
remove_gate_records() {
  rm -rf "$TEST_DIR/work/.putnami/sessions"/20000101-* "$TEST_DIR/work/.putnami/reports"/20000101-*
}
run_reusing_finalizer() {
  FINALIZER_UNDER_TEST="$TEST_DIR/verified/skills/fix/scripts/finalize-pr.sh" finalize \
    --title "fix(test): portable finalizer" --body-file "$TEST_DIR/body.md" \
    --proof-status passed --proof "real harness observed the expected behavior" \
    --project test/project "$@"
}
live_tree="$(cd "$TEST_DIR/work" && bash "$STUB_TREE_FINGERPRINT")"
write_gate_record 20000101-000001-reuse impacted success "$live_tree"
write_gate_record 20000101-000002-nested impacted success "$live_tree" 20000101-000001-reuse
write_gate_record 20000101-000003-red impacted failure "$live_tree"
write_gate_record 20000101-000004-projects projects success "$live_tree"
write_gate_record 20000101-000005-other impacted success "$(printf 'f%.0s' $(seq 64))"
write_gate_record 20000101-000006-noreport impacted success "$live_tree"
rm "$TEST_DIR/work/.putnami/reports/20000101-000006-noreport.json"
reused_output="$(run_reusing_finalizer 2>"$TEST_DIR/reused.err")"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$reused_output")" = "$PROPOSAL" ]
if [ -s "$TEST_DIR/state/gate-invocations" ]; then
  echo "finalize-pr test: the finalizer ran a gate on a tree whose impacted gate it could reuse:" >&2
  cat "$TEST_DIR/state/gate-invocations" >&2
  exit 1
fi
[ "$(cat "$TEST_DIR/state/gate-reuse-invocations")" = ".putnami/sessions/20000101-000001-reuse/session.json .putnami/reports/20000101-000001-reuse.json" ]
grep -Fq "reusing gate .putnami/sessions/20000101-000001-reuse/session.json" "$TEST_DIR/reused.err"
last_review |
  grep -Fq "lint, test, build, validate pass for test/project, then with --impacted in session 20000101-000001-reuse, reused on this tree"

# A newer candidate the checker refuses is named, and an older one it accepts
# is still reused.
reset_verification_logs
rm -f "$TEST_DIR/state/gate-reuse-invocations"
write_gate_record 20000101-000007-narrow impacted success "$live_tree"
printf '%s\n' .putnami/sessions/20000101-000007-narrow/session.json >"$TEST_DIR/state/reject-gate-reuse"
run_reusing_finalizer >/dev/null 2>"$TEST_DIR/fallthrough.err"
grep -Fq "gate .putnami/sessions/20000101-000007-narrow/session.json is not reusable: gate is narrower than the native unfiltered impacted plan" "$TEST_DIR/fallthrough.err"
grep -Fq "reusing gate .putnami/sessions/20000101-000001-reuse/session.json" "$TEST_DIR/fallthrough.err"
[ "$(wc -l <"$TEST_DIR/state/gate-reuse-invocations" | tr -d '[:space:]')" = 2 ]
[ ! -s "$TEST_DIR/state/gate-invocations" ]

# When the checker refuses every candidate, the finalizer gates itself, once.
reset_verification_logs
printf '*\n' >"$TEST_DIR/state/reject-gate-reuse"
refused_output="$(run_reusing_finalizer 2>"$TEST_DIR/refused-reuse.err")"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$refused_output")" = "$PROPOSAL" ]
grep -Fq "gate .putnami/sessions/20000101-000001-reuse/session.json is not reusable" "$TEST_DIR/refused-reuse.err"
[ "$(cat "$TEST_DIR/state/gate-invocations")" = "lint,test,build,validate --impacted --enforce-coverage" ]
rm -f "$TEST_DIR/state/reject-gate-reuse"

# A CLI older than `tree verify` reuses no gate, even one the checker would
# accept: the finalizer asks it about no candidate and gates itself, once.
reset_verification_logs
rm -f "$TEST_DIR/state/gate-reuse-invocations"
touch "$TEST_DIR/state/no-tree-verify"
old_cli_output="$(run_reusing_finalizer 2>"$TEST_DIR/old-cli-reuse.err")"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$old_cli_output")" = "$PROPOSAL" ]
[ ! -s "$TEST_DIR/state/gate-reuse-invocations" ]
if grep -Fq -e 'is not reusable' -e 'reusing gate' "$TEST_DIR/old-cli-reuse.err"; then
  echo "finalize-pr test: a CLI without tree verify still judged a gate for reuse:" >&2
  cat "$TEST_DIR/old-cli-reuse.err" >&2
  exit 1
fi
[ "$(cat "$TEST_DIR/state/gate-invocations")" = "lint,test,build,validate --impacted --enforce-coverage" ]
rm -f "$TEST_DIR/state/no-tree-verify"

# The gate and its retry carry the CI policy's --fix=false after
# --enforce-coverage, once, and no other flag of that policy. The policy file
# is ignored, so the tree the gate binds stays the committed one.
reset_verification_logs
printf '*\n' >"$TEST_DIR/state/reject-gate-reuse"
touch "$TEST_DIR/state/fail-gate-once"
mkdir -p "$TEST_DIR/work/.git/info"
printf 'putnami.ci.json\n' >>"$TEST_DIR/work/.git/info/exclude"
printf '%s\n' '{"version":3,"commands":["lint","test","build","validate"],"flags":["--fix=false","--continue-on-error","--enforce-coverage"]}' \
  >"$TEST_DIR/work/putnami.ci.json"
fix_false_output="$(run_reusing_finalizer 2>"$TEST_DIR/fix-false-gate.err")"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$fix_false_output")" = "$PROPOSAL" ]
if [ "$(cat "$TEST_DIR/state/gate-invocations")" != "$(printf '%s\n' \
  "lint,test,build,validate --impacted --enforce-coverage --fix=false" \
  "lint,test,build,validate --impacted --enforce-coverage --fix=false --retry-failed")" ]; then
  echo "finalize-pr test: the gate did not carry exactly the CI policy's --fix=false:" >&2
  cat "$TEST_DIR/state/gate-invocations" >&2
  exit 1
fi
printf '%s\n' '{"version":3,"commands":["lint"],"flags":"--fix=false"}' >"$TEST_DIR/work/putnami.ci.json"
reset_verification_logs
if run_reusing_finalizer >/dev/null 2>"$TEST_DIR/fix-false-flags.err"; then
  echo "finalize-pr test: expected a failure on a CI policy whose flags are not a list" >&2
  exit 1
fi
grep -Fq "putnami.ci.json is not readable JSON with a flags list" "$TEST_DIR/fix-false-flags.err"
[ ! -s "$TEST_DIR/state/gate-invocations" ]
rm -f "$TEST_DIR/work/putnami.ci.json" "$TEST_DIR/state/reject-gate-reuse"
remove_gate_records

# The usual /fix path: the coordinator gated a dirty tree, and the finalizer
# commits exactly the enumerated files of that tree without gating again.
reset_verification_logs
rm -f "$TEST_DIR/state/gate-reuse-invocations"
printf 'coordinator gated this\n' >"$TEST_DIR/work/example.txt"
write_gate_record 20000101-000008-dirty impacted success "$(cd "$TEST_DIR/work" && bash "$STUB_TREE_FINGERPRINT")"
dirty_head="$(git -C "$TEST_DIR/work" rev-parse HEAD)"
dirty_output="$(run_reusing_finalizer --file example.txt 2>"$TEST_DIR/dirty-reuse.err")"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$dirty_output")" = "$PROPOSAL" ]
grep -Fq "reusing gate .putnami/sessions/20000101-000008-dirty/session.json" "$TEST_DIR/dirty-reuse.err"
[ ! -s "$TEST_DIR/state/gate-invocations" ]
[ "$(git -C "$TEST_DIR/work" rev-parse HEAD^)" = "$dirty_head" ]
[ "$(git -C "$TEST_DIR/work" show HEAD:example.txt)" = "coordinator gated this" ]
[ -z "$(git -C "$TEST_DIR/work" status --porcelain)" ]
[ "$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)" = "$(git -C "$TEST_DIR/work" rev-parse HEAD)" ]
remove_gate_records

# A commit hook that rewrites a staged file publishes bytes no gate saw: the
# finalizer refuses before the push, and the next invocation finds no gate on
# the committed tree.
reset_verification_logs
# The hook lives in this test's own directory, named by the work repository's
# local config. `git rev-parse --git-path hooks` would follow a global
# core.hooksPath and install the hook for every repository on the machine.
hook_dir="$TEST_DIR/hooks"
mkdir -p "$hook_dir"
git -C "$TEST_DIR/work" config core.hooksPath "$hook_dir"
printf '#!/bin/sh\nprintf "hook edit\\n" >>example.txt\ngit add example.txt\n' >"$hook_dir/pre-commit"
chmod +x "$hook_dir/pre-commit"
printf 'gated before the hook\n' >"$TEST_DIR/work/example.txt"
write_gate_record 20000101-000009-hook impacted success "$(cd "$TEST_DIR/work" && bash "$STUB_TREE_FINGERPRINT")"
pushed_before_hook="$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)"
hook_head="$(git -C "$TEST_DIR/work" rev-parse HEAD)"
if run_reusing_finalizer --file example.txt >/dev/null 2>"$TEST_DIR/hook.err"; then
  echo "finalize-pr test: expected a refusal when a commit hook rewrites gated content" >&2
  exit 1
fi
grep -Fq "a commit hook changed the committed content after the gate; nothing was pushed" "$TEST_DIR/hook.err"
[ "$(git --git-dir="$TEST_DIR/origin.git" rev-parse refs/heads/fix/portable)" = "$pushed_before_hook" ]
git -C "$TEST_DIR/work" config --unset core.hooksPath
git -C "$TEST_DIR/work" reset -q --hard "$hook_head"
remove_gate_records

# A branch with nothing beyond origin/main is refused even while the local main
# is stale and would count the upstream commit as the branch's own.
git -C "$TEST_DIR/work" checkout -q -b fix/empty origin/main
[ "$(git -C "$TEST_DIR/work" rev-list --count main..HEAD)" -gt 0 ]
if (
  cd "$TEST_DIR/work"
  bash "$FINALIZER" --base main --branch fix/empty \
    --title "fix(test): nothing ahead" --body-file "$TEST_DIR/body.md" \
    --proof-status passed --proof "real harness observed the expected behavior" \
    --project test/project
) >"$TEST_DIR/empty-branch.out" 2>"$TEST_DIR/empty-branch.err"; then
  echo "finalize-pr test: expected a refusal for a branch with nothing ahead of origin/main" >&2
  exit 1
fi
grep -Fq "no commit ahead of 'origin/main'" "$TEST_DIR/empty-branch.err"
git -C "$TEST_DIR/work" checkout -q fix/portable

# Without a fetched origin/<base>, every comparison falls back to the local
# base branch, here brought up to date with the remote first.
reset_verification_logs
git -C "$TEST_DIR/work" branch -f main "$(git -C "$TEST_DIR/work" rev-parse origin/main)"
git -C "$TEST_DIR/work" update-ref -d refs/remotes/origin/main
fallback_output="$(EXPECTED_VERIFY_BASE=main run_verified_finalizer)"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$fallback_output")" = "$PROPOSAL" ]
[ -s "$TEST_DIR/state/verification-invocations" ]
[ ! -s "$TEST_DIR/state/gate-invocations" ]

# --draft publishes the branch when work starts: an empty commit carries the
# title, the proposal is a draft, no gate runs, nothing is commented and the
# task does not move.
git -C "$TEST_DIR/work" fetch -q origin
git -C "$TEST_DIR/work" checkout -q -b feat/draft origin/main
collab tasks transition "$(jq -cn --argjson ref "$TASK" '{ref: $ref, state: "in_progress"}')" >/dev/null
rm -f "$TEST_DIR/state/gate-invocations" "$TEST_DIR/state/collab-invocations" "$TEST_DIR/state/review-requests"
draft() {
  (
    cd "$TEST_DIR/work"
    bash "$FINALIZER" --draft --task-source "$TASK_SOURCE" --task-id "$TASK_ID" \
      --base main --branch feat/draft --title "feat(test): draft from the start" "$@"
  )
}
draft_output="$(draft --body-file "$TEST_DIR/body.md" 2>"$TEST_DIR/draft.err")"
DRAFT_PROPOSAL="$(sed -n 's/^PROPOSAL_REF=//p' <<<"$draft_output")"
[ -n "$DRAFT_PROPOSAL" ] && [ "$DRAFT_PROPOSAL" != "$PROPOSAL" ]
[ ! -s "$TEST_DIR/state/gate-invocations" ]
[ "$(git -C "$TEST_DIR/work" rev-list --count origin/main..HEAD)" = 1 ]
[ -z "$(git -C "$TEST_DIR/work" diff --name-only origin/main HEAD)" ]
[ "$(git -C "$TEST_DIR/work" log -1 --format=%s)" = "feat(test): draft from the start" ]
git -C "$TEST_DIR/work" log -1 --format=%B | grep -Fxq "Closes #$TASK_ID"
collab proposals status "$(jq -cn --argjson ref "$DRAFT_PROPOSAL" '{ref: $ref}')" >"$TEST_DIR/draft-status.json"
[ "$(jq -r '.proposal.state' "$TEST_DIR/draft-status.json")" = draft ]
[ "$(jq -r '.proposal.change.headCommit' "$TEST_DIR/draft-status.json")" = "$(git -C "$TEST_DIR/work" rev-parse HEAD)" ]
[ ! -s "$TEST_DIR/state/review-requests" ]
if grep -q '^tasks transition' "$TEST_DIR/state/collab-invocations"; then
  echo "finalize-pr test: --draft moved the task" >&2
  exit 1
fi
[ "$(task_state)" = in_progress ]

# Run again after a push, --draft republishes the body and makes no commit.
printf 'The draft states the net change of the branch.\n\nDeletes: nothing.\n' >"$TEST_DIR/draft-body.md"
[ "$(draft --body-file "$TEST_DIR/draft-body.md" 2>/dev/null | sed -n 's/^PROPOSAL_REF=//p')" = "$DRAFT_PROPOSAL" ]
[ "$(git -C "$TEST_DIR/work" rev-list --count origin/main..HEAD)" = 1 ]
collab proposals status "$(jq -cn --argjson ref "$DRAFT_PROPOSAL" '{ref: $ref}')" |
  jq -r '.proposal.body' | grep -Fxq "The draft states the net change of the branch."

# The host wraps the squash message itself: the published body joins the lines
# of each paragraph and list item, and keeps fenced blocks as written.
cat >"$TEST_DIR/wrapped-body.md" <<'WRAPPED'
The host wraps the squash message,
so the finalizer joins   
a wrapped paragraph.

- a list item that
  continues here
1. a numbered item

```text
kept
  as is
```

    indented code
    stays

Deletes: nothing.
WRAPPED
cat >"$TEST_DIR/unwrapped-body.md" <<UNWRAPPED
The host wraps the squash message, so the finalizer joins a wrapped paragraph.

- a list item that continues here
1. a numbered item

\`\`\`text
kept
  as is
\`\`\`

    indented code
    stays

Deletes: nothing.

Closes #$TASK_ID
UNWRAPPED
draft --body-file "$TEST_DIR/wrapped-body.md" >/dev/null 2>&1
collab proposals status "$(jq -cn --argjson ref "$DRAFT_PROPOSAL" '{ref: $ref}')" | jq -r '.proposal.body' >"$TEST_DIR/published-body.md"
diff -u "$TEST_DIR/unwrapped-body.md" "$TEST_DIR/published-body.md" ||
  { echo "finalize-pr test: the published body was not unwrapped" >&2; exit 1; }

# --draft gates nothing, so it takes no gate argument, and it commits nothing
# that is staged.
if draft --body-file "$TEST_DIR/body.md" --proof-status passed --proof "x" >/dev/null 2>"$TEST_DIR/draft-args.err"; then
  echo "finalize-pr test: --draft accepted a proof" >&2
  exit 1
fi
grep -Fq "it takes no proof, project, file, gate or verification argument" "$TEST_DIR/draft-args.err"
printf 'staged\n' >"$TEST_DIR/work/staged.txt"
git -C "$TEST_DIR/work" add staged.txt
if draft --body-file "$TEST_DIR/body.md" >/dev/null 2>"$TEST_DIR/draft-staged.err"; then
  echo "finalize-pr test: --draft committed a staged path" >&2
  exit 1
fi
grep -Fq -- "--draft commits nothing that is staged" "$TEST_DIR/draft-staged.err"
git -C "$TEST_DIR/work" rm -q --cached staged.txt
rm "$TEST_DIR/work/staged.txt"

# The hosted gate. The policy names the checks that stand for the gate; the
# local provider runs none, so the finalizer returns the proposal to draft.
hosted_policy() {
  jq --argjson timeout "$1" '.options["@putnami/contributor"].verification.ciGate = {checks: ["Putnami CI"], pollSeconds: 0, timeoutMinutes: $timeout}' \
    "$TEST_DIR/workspace.json.plain" >"$TEST_DIR/work/putnami.workspace.json"
  git -C "$TEST_DIR/work" commit -q -am "chore(test): hosted checks stand for the gate"
}
hosted() {
  (
    cd "$TEST_DIR/work"
    bash "${FINALIZER_UNDER_TEST:-$FINALIZER}" --task-source "$TASK_SOURCE" --task-id "$TASK_ID" \
      --base main --branch feat/draft --title "feat(test): draft from the start" --body-file "$TEST_DIR/draft-body.md" \
      --proof-status not-applicable --proof "the hosted checks are the proof" --project test/project "$@"
  )
}
hosted_policy 1
printf 'hosted\n' >"$TEST_DIR/work/example.txt"
rm -f "$TEST_DIR/state/gate-invocations" "$TEST_DIR/state/collab-invocations"
if hosted --gate ci --file example.txt >/dev/null 2>"$TEST_DIR/hosted-unsupported.err"; then
  echo "finalize-pr test: expected a provider without hosted checks to fail the hosted gate" >&2
  exit 1
fi
grep -Fq "the proposals provider runs no hosted checks; finalize with --gate local. The proposal is a draft again" "$TEST_DIR/hosted-unsupported.err"
[ ! -s "$TEST_DIR/state/gate-invocations" ]
[ "$(collab proposals status "$(jq -cn --argjson ref "$DRAFT_PROPOSAL" '{ref: $ref}')" | jq -r '.proposal.state')" = draft ]
grep -Fxq "proposals upsert --input-file - --output=json" "$TEST_DIR/state/collab-invocations"
[ "$(task_state)" = in_progress ]

# Checks that pass after a while: the finalizer waits for every named one on
# the pushed commit, then comments and delivers. Other checks do not count.
pending='{"state":"pending","items":[{"name":"Putnami CI","state":"pending"}]}'
passing='{"state":"failing","items":[{"name":"Putnami CI","state":"failing","url":"https://ci.example/run/0"},{"name":"Putnami CI","state":"passing","url":"https://ci.example/run/1"},{"name":"other","state":"failing"}]}'
printf '%s\n%s\n%s\n' '{"state":"none"}' "$pending" "$passing" >"$TEST_DIR/state/hosted-checks"
rm -f "$TEST_DIR/state/gate-invocations" "$TEST_DIR/state/review-requests"
hosted_output="$(hosted --gate ci 2>"$TEST_DIR/hosted-pass.err")"
[ "$(sed -n 's/^PROPOSAL_REF=//p' <<<"$hosted_output")" = "$DRAFT_PROPOSAL" ]
[ ! -s "$TEST_DIR/state/gate-invocations" ]
[ "$(collab proposals status "$(jq -cn --argjson ref "$DRAFT_PROPOSAL" '{ref: $ref}')" | jq -r '.proposal.state')" = open ]
last_review | grep -Fq "ordinary gate: hosted checks passed on $(git -C "$TEST_DIR/work" rev-parse HEAD): Putnami CI https://ci.example/run/1"
[ "$(task_state)" = blocked ]

# A failing check returns the proposal to draft and names the check.
collab tasks transition "$(jq -cn --argjson ref "$TASK" '{ref: $ref, state: "in_progress"}')" >/dev/null
printf '%s\n' '{"state":"failing","items":[{"name":"Putnami CI","state":"failing","url":"https://ci.example/run/2"}]}' >"$TEST_DIR/state/hosted-checks"
printf 'hosted again\n' >"$TEST_DIR/work/example.txt"
if hosted --gate ci --file example.txt >/dev/null 2>"$TEST_DIR/hosted-fail.err"; then
  echo "finalize-pr test: expected a failing hosted check to fail the finalizer" >&2
  exit 1
fi
grep -Fq "hosted checks failed on $(git -C "$TEST_DIR/work" rev-parse HEAD): Putnami CI https://ci.example/run/2. The proposal is a draft again" "$TEST_DIR/hosted-fail.err"
[ "$(collab proposals status "$(jq -cn --argjson ref "$DRAFT_PROPOSAL" '{ref: $ref}')" | jq -r '.proposal.state')" = draft ]
[ "$(task_state)" = in_progress ]

# --gate auto leaves the gate to the hosted checks on a loaded machine, and
# runs it locally otherwise.
mkdir -p "$TEST_DIR/loaded/skills/fix/scripts" "$TEST_DIR/loaded/skills/check/scripts"
cp "$FINALIZER" "$ROOT/.agents/skills/fix/scripts/tree-fingerprint.sh" "$TEST_DIR/loaded/skills/fix/scripts/"
cp "$ROOT/.agents/skills/check/scripts/english-only.sh" "$TEST_DIR/loaded/skills/check/scripts/"
printf '#!/usr/bin/env bash\necho 0.95\n' >"$TEST_DIR/loaded/skills/fix/scripts/machine-load.sh"
printf '%s\n' "$passing" >"$TEST_DIR/state/hosted-checks"
rm -f "$TEST_DIR/state/gate-invocations"
FINALIZER_UNDER_TEST="$TEST_DIR/loaded/skills/fix/scripts/finalize-pr.sh" hosted >/dev/null 2>"$TEST_DIR/hosted-auto.err"
grep -Fq "machine load 0.95 is above 0.7; the hosted checks gate this change instead of a local run" "$TEST_DIR/hosted-auto.err"
[ ! -s "$TEST_DIR/state/gate-invocations" ]
printf '#!/usr/bin/env bash\necho 0.20\n' >"$TEST_DIR/loaded/skills/fix/scripts/machine-load.sh"
printf 'idle\n' >"$TEST_DIR/work/example.txt"
FINALIZER_UNDER_TEST="$TEST_DIR/loaded/skills/fix/scripts/finalize-pr.sh" hosted --file example.txt >/dev/null 2>"$TEST_DIR/hosted-idle.err"
[ "$(cat "$TEST_DIR/state/gate-invocations")" = "lint,test,build,validate --impacted --enforce-coverage" ]

# Checks still pending when the policy's timeout ends stop the finalizer; the
# proposal stays ready and the next invocation keeps waiting.
hosted_policy 0
printf '%s\n' "$pending" >"$TEST_DIR/state/hosted-checks"
if hosted --gate ci >/dev/null 2>"$TEST_DIR/hosted-timeout.err"; then
  echo "finalize-pr test: expected pending hosted checks to time out" >&2
  exit 1
fi
grep -Fq "after 0 minutes; they keep running. Invoke the finalizer again to keep waiting" "$TEST_DIR/hosted-timeout.err"
[ "$(collab proposals status "$(jq -cn --argjson ref "$DRAFT_PROPOSAL" '{ref: $ref}')" | jq -r '.proposal.state')" = open ]
rm -f "$TEST_DIR/state/hosted-checks"

# No step of any run reached a backend client directly.
[ ! -s "$TEST_DIR/state/gh-invocations" ]

echo "finalize-pr test: ok"
