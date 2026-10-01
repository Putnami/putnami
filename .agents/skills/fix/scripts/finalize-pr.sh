#!/usr/bin/env bash
# Commit an explicitly enumerated change, push it with Git, publish its change
# proposal through the workspace's proposals provider, and move its task through
# the workspace's tasks provider. Both Claude Code and Codex call this helper.
# It never talks to a backend directly: every task and proposal operation is a
# `putnami tasks|proposals <operation>` call routed to the bound provider.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  bash finalize-pr.sh --base <branch> --branch <branch> \
    --title <commit-and-proposal-title> --body-file <path> \
    --proof-status <passed|not-applicable> --proof <evidence-or-reason> \
    --project <name> [--project <name> ...] \
    --file <path> [--file <path> ...] \
    [--task-source <source> --task-id <id>] [--verification-file <record.json>] \
    [--gate auto|local|ci]
  bash finalize-pr.sh --draft --base <branch> --branch <branch> \
    --title <commit-and-proposal-title> --body-file <path> \
    [--task-source <source> --task-id <id>]

--draft publishes the branch as a draft proposal when work starts, before any
gate: it commits nothing but an empty commit carrying the title when the
branch has no commit ahead of the base, pushes, and upserts the proposal as a
draft. Run it again to republish the title and body after a push. Without
--draft, the helper finalizes: it gates, commits, pushes and marks the
proposal ready for review.

The proposal body is the squash commit message: the caller's body plus the
task reference line, nothing else. The helper joins the lines of each
paragraph and list item, because the host wraps the squash message itself;
blank lines, list markers and fenced blocks keep their breaks. The body has
no markdown heading, fits the
policy's publication.bodyMaxBytes, and no title, body, proof or commit names
an agent (a Co-Authored-By trailer or a "Generated with" line for a model or
an agent host). The verification record goes to a comment on the proposal,
through `proposals review` with the comment verdict.

The helper is resumable: if the branch is already committed or its proposal
already exists, it verifies and completes the remaining lifecycle operations.
It runs the Putnami gate with --impacted, once when that run is green and once
more with --retry-failed when it is red, so a replayed failure cannot wedge it.
It runs no gate when a green --impacted or all-project gate already ran on this
exact tree and `putnami tree verify --gate` accepts it: same
fingerprint, enforced coverage, and no task of the native impacted plan missing.

--gate ci leaves the gate to the hosted checks the policy names in
verification.ciGate.checks: the helper pushes, marks the proposal ready, which
starts them, and waits for every named check to pass on the pushed commit.
A failing check returns the proposal to draft. --gate auto, the default, takes
that path when no gate is reusable and this machine's load ratio, which
"bash machine-load.sh" prints, exceeds verification.ciGate.load (default 0.7);
otherwise it gates locally.

Publication goes through the collaboration contracts of the workspace:
`proposals upsert` for the exact base and head, read back with
`proposals status`, then, when --task-source and --task-id name a task,
`tasks transition` to the repository policy's delivered state
(options["@putnami/contributor"].states.delivered, default in_progress). The
transition is sent even when the task already reads as that state: the
provider decides whether its own representation still has to change.
Before the gate, the bound providers must offer every operation this helper
calls. Every request goes to the CLI on standard input, and the proposal body
must fit the contract's 65536 bytes before anything is pushed.
It prints PROPOSAL_REF=<reference> (and PROPOSAL_URL=<url> when the provider
supplies one) only after the caller-supplied local-proof status and evidence, the
proposal and the task state are verified. What it verifies is what the
contracts return: the proposal's base, head, head commit and state; its
assignees and labels, including those the provider applies from its own
settings, which the proposal read back must carry as the upsert answered them;
and the task's state. A provider that does not report assignees or labels is
not verified on them, and the helper says so.
Missing or blocked proof must not use this ready finalizer. An unresolved
provider write stops the helper without a retry and names the read that
reconciles it.

With --verification-file, the branch must already be committed and clean.
`putnami tree verify` must accept the dossier on the current tree before any
mutation. Its applicable gate, proof and review replace this helper's gate;
caller-supplied --proof prose does not establish a verified verdict.
A Putnami CLI without `tree verify` refuses --verification-file and reuses
no gate.

--project does not select the gate: the projects the change touched were gated
by whoever implemented it, and this helper records their names in the
verification comment.
USAGE
}

BASE=""
BRANCH=""
TITLE=""
BODY_FILE=""
PROOF_STATUS=""
PROOF=""
VERIFICATION_FILE=""
TASK_SOURCE=""
TASK_ID=""
DRAFT_STAGE=false
GATE_MODE=auto
FILES=()
PROJECTS=()

while [ "$#" -gt 0 ]; do
  case "$1" in
    --issue)
      echo "finalize-pr: --issue is replaced by --task-source and --task-id: pass the task reference the tasks provider returned" >&2
      exit 2
      ;;
    --task-source) TASK_SOURCE="${2:?--task-source needs a value}"; shift ;;
    --task-id) TASK_ID="${2:?--task-id needs a value}"; shift ;;
    --base) BASE="${2:?--base needs a value}"; shift ;;
    --branch) BRANCH="${2:?--branch needs a value}"; shift ;;
    --title) TITLE="${2:?--title needs a value}"; shift ;;
    --body-file) BODY_FILE="${2:?--body-file needs a value}"; shift ;;
    --proof-status) PROOF_STATUS="${2:?--proof-status needs a value}"; shift ;;
    --proof) PROOF="${2:?--proof needs a value}"; shift ;;
    --verification-file) VERIFICATION_FILE="${2:?--verification-file needs a value}"; shift ;;
    --project) PROJECTS+=("${2:?--project needs a value}"); shift ;;
    --file) FILES+=("${2:?--file needs a value}"); shift ;;
    --co-author)
      echo "finalize-pr: --co-author is gone: no commit, proposal or comment names an agent" >&2
      exit 2
      ;;
    --draft) DRAFT_STAGE=true ;;
    --gate) GATE_MODE="${2:?--gate needs a value}"; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "finalize-pr: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

REQUIRED=(BASE BRANCH TITLE BODY_FILE)
[ "$DRAFT_STAGE" = true ] || REQUIRED+=(PROOF_STATUS PROOF)
for required in "${REQUIRED[@]}"; do
  if [ -z "${!required}" ]; then
    echo "finalize-pr: --$(printf '%s' "$required" | tr '[:upper:]_' '[:lower:]-') is required" >&2
    exit 2
  fi
done
case "$GATE_MODE" in
  auto|local|ci) ;;
  *) echo "finalize-pr: --gate must be auto, local or ci" >&2; exit 2 ;;
esac
if [ "$DRAFT_STAGE" = true ]; then
  if [ -n "$PROOF_STATUS$PROOF$VERIFICATION_FILE" ] || [ "${#PROJECTS[@]}" -gt 0 ] || [ "${#FILES[@]}" -gt 0 ] || [ "$GATE_MODE" != auto ]; then
    echo "finalize-pr: --draft publishes commits as they are: it takes no proof, project, file, gate or verification argument" >&2
    exit 2
  fi
else
  case "$PROOF_STATUS" in
    passed|not-applicable) ;;
    *) echo "finalize-pr: --proof-status must be passed or not-applicable" >&2; exit 2 ;;
  esac
  case "$PROOF" in
    *[![:space:]]*) ;;
    *) echo "finalize-pr: --proof must contain evidence or a not-applicable reason" >&2; exit 2 ;;
  esac
  [ "${#PROJECTS[@]}" -gt 0 ] || { echo "finalize-pr: at least one --project is required" >&2; exit 2; }
  if [ "$GATE_MODE" = ci ] && [ -n "$VERIFICATION_FILE" ]; then
    echo "finalize-pr: --gate ci and --verification-file both name the gate; pass one" >&2
    exit 2
  fi
fi

# Every revision comparison uses the remote base: the proposal targets it, and a
# local base branch can be stale, for example when another worktree owns it.
# A stale base counts commits other people already landed, and the verifier
# rejects a dossier bound to the real base.
REVISION_BASE="$BASE"
if git rev-parse --verify -q "refs/remotes/origin/$BASE" >/dev/null; then
  REVISION_BASE="origin/$BASE"
fi
if [ -n "$TASK_SOURCE$TASK_ID" ] && { [ -z "$TASK_SOURCE" ] || [ -z "$TASK_ID" ]; }; then
  echo "finalize-pr: --task-source and --task-id name one task reference together; pass both or neither" >&2
  exit 2
fi
case "$TASK_SOURCE$TASK_ID" in
  *[[:space:]]*) echo "finalize-pr: a task reference contains no whitespace" >&2; exit 2 ;;
esac
[ -s "$BODY_FILE" ] || { echo "finalize-pr: body file is missing or empty: $BODY_FILE" >&2; exit 2; }

command -v git >/dev/null || { echo "finalize-pr: git not found" >&2; exit 1; }
# jq reads the repository policy, every provider envelope and the gate's own
# session record below.
command -v jq >/dev/null || { echo "finalize-pr: jq not found" >&2; exit 1; }
# Under Git for Windows a native jq.exe ends every output line with CRLF, and
# the CR left in a captured value fails every comparison; --binary keeps LF.
case "${OSTYPE:-}" in
  msys* | cygwin*) jq() { command jq --binary "$@"; } ;;
esac

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

# A Conventional Commit title names what the diff changes, not the skill that
# produced it: a change that touches only tests is `test`, not `fix` or `feat`.
if [[ "$TITLE" =~ ^(fix|feat)(\([^\)]*\))?!?: ]]; then
  if [ "${#FILES[@]}" -gt 0 ]; then
    CHANGED_PATHS="$(printf '%s\n' "${FILES[@]}")"
  else
    CHANGED_PATHS="$(git diff --name-only "$REVISION_BASE...HEAD" 2>/dev/null || true)"
  fi
  TEST_PATH_PATTERN='(^|/)(testdata|__tests__|tests?)/|_test\.(go|py)$|(^|/)test_[^/]*\.py$|\.(test|spec)\.[cm]?[jt]sx?$|\.test\.sh$'
  if [ -n "$CHANGED_PATHS" ] && ! grep -Evq "$TEST_PATH_PATTERN" <<<"$CHANGED_PATHS"; then
    echo "finalize-pr: every changed path is a test, so the title type is test, not ${BASH_REMATCH[1]}: retitle it \"test${TITLE#"${BASH_REMATCH[1]}"}\"" >&2
    exit 2
  fi
fi

if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
elif command -v putnami >/dev/null 2>&1; then
  PUTNAMI_CLI=putnami
else
  echo "finalize-pr: no Putnami CLI found — expected ./putnamiw or putnami on PATH" >&2
  exit 1
fi

# The repository policy: conventions only, never credentials or provider
# settings. Every member is optional.
POLICY='{}'
if [ -f putnami.workspace.json ]; then
  if ! POLICY="$(jq -ce '.options["@putnami/contributor"] // {}' putnami.workspace.json)"; then
    echo "finalize-pr: putnami.workspace.json is not readable JSON" >&2
    exit 1
  fi
fi
POLICY_VERSION="$(jq -r '.version // 1' <<<"$POLICY")"
[ "$POLICY_VERSION" = 1 ] || {
  echo "finalize-pr: repository policy version $POLICY_VERSION is not supported by this helper (it reads version 1)" >&2
  exit 1
}
DELIVERED_STATE="$(jq -r '.states.delivered // "in_progress"' <<<"$POLICY")"
case "$DELIVERED_STATE" in
  open|in_progress|blocked|done|canceled) ;;
  *) echo "finalize-pr: policy states.delivered must be a task state (open, in_progress, blocked, done, canceled): $DELIVERED_STATE" >&2; exit 1 ;;
esac
DRAFT="$(jq -r 'if .publication.draft == true then "true" else "false" end' <<<"$POLICY")"
TASK_REFERENCE_TEMPLATE="$(jq -r '.publication.taskReference // ""' <<<"$POLICY")"
LANGUAGE="$(jq -r '.verification.language // ""' <<<"$POLICY")"
BODY_MAX_BYTES="$(jq -r '.publication.bodyMaxBytes // 0' <<<"$POLICY")"
CI_CHECKS="$(jq -c '[.verification.ciGate.checks // [] | .[] | strings]' <<<"$POLICY")"
CI_LOAD="$(jq -r '.verification.ciGate.load // 0.7' <<<"$POLICY")"
CI_TIMEOUT_MINUTES="$(jq -r '.verification.ciGate.timeoutMinutes // 60' <<<"$POLICY")"
CI_POLL_SECONDS="$(jq -r '.verification.ciGate.pollSeconds // 30' <<<"$POLICY")"
for number in BODY_MAX_BYTES CI_TIMEOUT_MINUTES CI_POLL_SECONDS; do
  case "${!number}" in
    '' | *[!0-9]*) echo "finalize-pr: policy $number must be a whole number: ${!number}" >&2; exit 1 ;;
  esac
done
case "$CI_LOAD" in
  '' | *[!0-9.]*) echo "finalize-pr: policy verification.ciGate.load must be a number: $CI_LOAD" >&2; exit 1 ;;
esac
TASK_REFERENCE_LINE=""
if [ -n "$TASK_ID" ] && [ -n "$TASK_REFERENCE_TEMPLATE" ]; then
  TASK_REFERENCE_LINE="${TASK_REFERENCE_TEMPLATE//\{id\}/$TASK_ID}"
fi

# The body becomes the squash commit message: no heading, and no longer than
# the policy allows. Evidence goes to the verification comment, not here.
if grep -Eq '^#{1,6}[[:space:]]' "$BODY_FILE"; then
  echo "finalize-pr: the body is the squash commit message and carries no markdown heading; write plain paragraphs and lists: $(grep -Em1 '^#{1,6}[[:space:]]' "$BODY_FILE")" >&2
  exit 1
fi

# No published text names an agent: no Co-Authored-By trailer or "Generated
# with" line for a model or an agent host. A human co-author stays allowed.
AGENT_MARKER='^[[:space:]]*co-authored-by:.*(anthropic|openai|claude|codex|gpt-[0-9])|generated (with|by) \[?(claude|codex|chatgpt|openai|anthropic)'
# grep reads its whole input: one that stopped at the first match would leave
# the writer to die of SIGPIPE, and pipefail would end the run without a word.
agent_marker_in() {
  { grep -Ei "$AGENT_MARKER" || true; } | sed -n 1p
}
MARKER="$({ printf '%s\n\n' "$TITLE"; cat "$BODY_FILE"; printf '\n%s\n' "$PROOF"; } | agent_marker_in)"
if [ -n "$MARKER" ]; then
  echo "finalize-pr: the title, body or proof names an agent; remove it: $MARKER" >&2
  exit 1
fi
BODY_FILE_BYTES="$(wc -c <"$BODY_FILE" | tr -d '[:space:]')"
if [ "$BODY_MAX_BYTES" -gt 0 ] && [ "$BODY_FILE_BYTES" -gt "$BODY_MAX_BYTES" ]; then
  echo "finalize-pr: the body is $BODY_FILE_BYTES bytes and the policy allows $BODY_MAX_BYTES (publication.bodyMaxBytes); state the net change in fewer words" >&2
  exit 1
fi

# A repository that sets the English language rule must not publish a proposal
# in another language. The title is also the commit subject, and the proof goes
# into the proposal body: one check covers the commit, the title and the body.
case "$LANGUAGE" in
  "") ;;
  en)
    ENGLISH_ONLY="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../check/scripts" 2>/dev/null && pwd || true)/english-only.sh"
    [ -f "$ENGLISH_ONLY" ] || { echo "finalize-pr: English-only detector not found: $ENGLISH_ONLY" >&2; exit 1; }
    ENGLISH_STATUS=0
    { printf '%s\n\n' "$TITLE"; cat "$BODY_FILE"; printf '\n%s\n' "$PROOF"; } |
      bash "$ENGLISH_ONLY" text --label "proposal title, body and proof" >&2 || ENGLISH_STATUS=$?
    case "$ENGLISH_STATUS" in
      0) ;;
      1) echo "finalize-pr: the proposal title, body or proof is not in English; rewrite it in English, then finalize again" >&2; exit 1 ;;
      *) echo "finalize-pr: the English-only check failed (exit $ENGLISH_STATUS)" >&2; exit 1 ;;
    esac
    ;;
  *) echo "finalize-pr: policy verification.language '$LANGUAGE' has no detector; only en is supported" >&2; exit 1 ;;
esac

CURRENT_BRANCH="$(git branch --show-current)"
if [ "$CURRENT_BRANCH" != "$BRANCH" ]; then
  echo "finalize-pr: current branch '$CURRENT_BRANCH' is not expected branch '$BRANCH'" >&2
  exit 1
fi

# The fingerprint is the sibling script's, not a second copy of it: the
# orchestrator and its workers compare fingerprints across agents, and two
# implementations of the same idea would compare nothing.
TREE_FINGERPRINT="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)/tree-fingerprint.sh"
[ -f "$TREE_FINGERPRINT" ] || {
  echo "finalize-pr: tree fingerprint helper not found: $TREE_FINGERPRINT" >&2
  exit 1
}

tree_fingerprint() {
  bash "$TREE_FINGERPRINT"
}

# collab <contract> <operation> <request-json> calls one contract operation and
# leaves its envelope in COLLAB_ENVELOPE and its outcome in COLLAB_OUTCOME. The
# request goes on standard input, never on the command line, so its size is
# the contract's limit and not the operating system's argument limit. A call
# that printed no envelope is a tool failure, never an outcome.
COLLAB_ENVELOPE=""
COLLAB_OUTCOME=""
collab() {
  local contract="$1" operation="$2" request="$3" status=0
  COLLAB_ENVELOPE="$(printf '%s' "$request" | "$PUTNAMI_CLI" "$contract" "$operation" --input-file - --output=json)" || status=$?
  if ! COLLAB_OUTCOME="$(jq -er '.outcome' <<<"$COLLAB_ENVELOPE" 2>/dev/null)"; then
    echo "finalize-pr: \`$PUTNAMI_CLI $contract $operation\` printed no collaboration envelope (exit $status): the workspace declares no options.collaboration block in putnami.workspace.json, or the CLI predates the collaboration contracts" >&2
    exit 1
  fi
}

# require_ok <what> stops on any outcome but ok. An unresolved write is never
# repeated here: its reconcile read decides whether it happened.
require_ok() {
  local what="$1" reason message reconcile
  [ "$COLLAB_OUTCOME" = ok ] && return 0
  reason="$(jq -r '.error.reason // ""' <<<"$COLLAB_ENVELOPE")"
  message="$(jq -r '.error.message // ""' <<<"$COLLAB_ENVELOPE")"
  if [ "$COLLAB_OUTCOME" = unresolved ]; then
    reconcile="$(jq -r '.error.reconcile // ""' <<<"$COLLAB_ENVELOPE")"
    echo "finalize-pr: $what is unresolved ($reason): $message" >&2
    echo "finalize-pr: the write may have happened; reconcile with: ${reconcile:-a read of the same item}, then finalize again. Nothing was retried." >&2
    exit 1
  fi
  echo "finalize-pr: $what failed: $COLLAB_OUTCOME ($reason): $message" >&2
  exit 1
}

# require_supported <contract> <operation>... stops when the capabilities
# envelope in COLLAB_ENVELOPE does not offer an operation this helper calls.
require_supported() {
  local contract="$1" operation
  shift
  for operation in "$@"; do
    if [ "$(jq -r --arg operation "$operation" '[.result.operations[]? | select(.name == $operation) | .supported][0] // false' <<<"$COLLAB_ENVELOPE")" != true ]; then
      echo "finalize-pr: the bound $contract provider does not offer $operation, which this helper calls; nothing was written" >&2
      exit 1
    fi
  done
}

# precondition_mode <contract> <operation> prints how the bound provider
# enforces expectedRevision for an operation: atomic, checked or none.
precondition_mode() {
  jq -r --arg operation "$2" '[.result.operations[] | select(.name == $operation) | .preconditions // "none"][0] // "none"' <<<"$1"
}

# The execute verifier is the CLI's `tree verify`. A consumer CLI older than
# that command does not have it. Its --help is no probe, because an older CLI
# answers any --help with exit 0; a run without arguments is one, because the
# command writes a JSON verdict for every failure, a usage error included, and
# an older CLI writes nothing to stdout. The answer is kept for the next call.
TREE_VERIFY=""
has_tree_verify() {
  if [ -z "$TREE_VERIFY" ]; then
    local probe
    probe="$("$PUTNAMI_CLI" tree verify 2>/dev/null || true)"
    if jq -e 'type == "object" and has("verdict")' >/dev/null 2>&1 <<<"$probe"; then
      TREE_VERIFY=yes
    else
      TREE_VERIFY=no
    fi
  fi
  [ "$TREE_VERIFY" = yes ]
}

verify_dossier() {
  [ -n "$VERIFICATION_FILE" ] || return 0
  if [ -n "$(git status --porcelain)" ]; then
    echo "finalize-pr: verification mode requires a clean, committed tree; checkpoint changes and renew their evidence" >&2
    exit 1
  fi
  local verified_head
  verified_head="$(git rev-parse HEAD)"
  has_tree_verify || {
    echo "finalize-pr: --verification-file needs \`putnami tree verify\`, and $PUTNAMI_CLI does not have it; upgrade the Putnami CLI, then finalize again" >&2
    exit 1
  }
  "$PUTNAMI_CLI" tree verify --record "$VERIFICATION_FILE" --base "$REVISION_BASE" >&2
  # The verifier reads evidence; it must not change the tree it accepted.
  [ -z "$(git status --porcelain)" ] && [ "$(git rev-parse HEAD)" = "$verified_head" ] || {
    echo "finalize-pr: tree changed during verification" >&2
    exit 1
  }
}

# Gate records are the sessions whose commands are exactly the four gate
# commands. The filter is not cosmetic: a gate spawns nested sessions of its own
# (validate's clientgen guard runs a build) whose ids sort AFTER their parent's,
# so "the newest record" is reliably the wrong one.
#
# `find` exits non-zero in a worktree that has never run a command, and a record
# that is not a gate leaves the loop's last status non-zero: neither is an error,
# so both are absorbed rather than left to `set -e`.
gate_records() {
  { find .putnami/sessions -mindepth 2 -maxdepth 2 -name session.json 2>/dev/null || true; } | sort |
    while IFS= read -r record; do
      if jq -e '.commands == ["lint","test","build","validate"]' "$record" >/dev/null 2>&1; then
        printf '%s\n' "$record"
      fi
    done
}

# reusable_gate prints the record of a green --impacted or all-project gate that
# already ran on the live tree, when `putnami tree verify --gate` accepts it. The
# coordinator's final gate is the usual one: running a second gate on the same
# tree would re-plan and re-hash hundreds of tasks to learn nothing. Newest
# first; the verifier, not this filter, decides. A CLI without `tree verify`
# reuses nothing, and this helper runs its own gate.
reusable_gate() {
  has_tree_verify || return 0
  local live records record report verdict reason
  live="$(tree_fingerprint)"
  # Read into a variable first: leaving a pipe early would kill its writer
  # with SIGPIPE, which pipefail turns into this helper's own failure.
  records="$({ find .putnami/sessions -mindepth 2 -maxdepth 2 -name session.json 2>/dev/null || true; } | sort -r)"
  while IFS= read -r record; do
    [ -n "$record" ] || continue
    jq -e --arg tree "$live" '(.parentSessionId // "") == ""
      and (.selection.mode == "impacted" or .selection.mode == "all")
      and .run.outcome == "success" and .tree.fingerprint == $tree' "$record" >/dev/null 2>&1 || continue
    report=".putnami/reports/$(jq -r '.sessionId' "$record").json"
    [ -f "$report" ] || continue
    if verdict="$("$PUTNAMI_CLI" tree verify --gate "$record" --report "$report" --base "$REVISION_BASE" 2>&1)"; then
      printf '%s\n' "$record"
      return 0
    fi
    # The verifier prints its reason as JSON; anything else is a crash, shown whole.
    reason="$(jq -r '.reason // empty' <<<"$verdict" 2>/dev/null || true)"
    echo "finalize-pr: gate $record is not reusable: ${reason:-${verdict:-the verifier printed nothing}}" >&2
  done <<<"$records"
}

contains_file() {
  local wanted="$1"
  local candidate
  [ "${#FILES[@]}" -gt 0 ] || return 1
  for candidate in "${FILES[@]}"; do
    [ "$candidate" = "$wanted" ] && return 0
  done
  return 1
}

# Preflight, before anything is written: the proposals contract must be bound
# and offer every operation this helper calls, and so must the tasks contract
# when a task is named, which must exist. Reads only.
collab proposals capabilities '{}'
require_ok "reading the proposals binding"
if [ "$(jq -r '.result.status' <<<"$COLLAB_ENVELOPE")" != bound ]; then
  echo "finalize-pr: the proposals contract is $(jq -r '.result.status' <<<"$COLLAB_ENVELOPE") in this workspace; bind a provider in options.collaboration.proposals of putnami.workspace.json" >&2
  exit 1
fi
require_supported proposals upsert status
REVIEW_SUPPORTED="$(jq -r '[.result.operations[]? | select(.name == "review") | .supported][0] // false' <<<"$COLLAB_ENVELOPE")"
TASK_REF=""
TASK_PRECONDITIONS=none
if [ -n "$TASK_ID" ]; then
  TASK_REF="$(jq -cn --arg source "$TASK_SOURCE" --arg id "$TASK_ID" '{source: $source, id: $id}')"
  collab tasks capabilities '{}'
  require_ok "reading the tasks binding"
  if [ "$(jq -r '.result.status' <<<"$COLLAB_ENVELOPE")" != bound ]; then
    echo "finalize-pr: the tasks contract is $(jq -r '.result.status' <<<"$COLLAB_ENVELOPE") in this workspace; bind a provider in options.collaboration.tasks of putnami.workspace.json, or finalize without a task" >&2
    exit 1
  fi
  if [ "$DRAFT_STAGE" = true ]; then
    require_supported tasks get
  else
    require_supported tasks get transition
  fi
  TASK_PRECONDITIONS="$(precondition_mode "$COLLAB_ENVELOPE" transition)"
  collab tasks get "$(jq -cn --argjson ref "$TASK_REF" '{ref: $ref}')"
  require_ok "reading task $TASK_ID"
fi

# Never absorb pre-staged work that the orchestrator did not enumerate.
if ! STAGED_PATHS="$(git diff --cached --name-only)"; then
  echo "finalize-pr: failed to inspect pre-staged paths" >&2
  exit 1
fi
while IFS= read -r staged; do
  [ -z "$staged" ] && continue
  if [ "$DRAFT_STAGE" = true ]; then
    echo "finalize-pr: --draft commits nothing that is staged; commit the checkpoint or unstage it: $staged" >&2
    exit 1
  fi
  if ! contains_file "$staged"; then
    echo "finalize-pr: staged path was not passed with --file: $staged" >&2
    exit 1
  fi
done <<<"$STAGED_PATHS"

# The gate: the dossier's, a reusable one, the hosted checks, or a local run.
REUSED_GATE=""
CI_GATE=false
if [ "$DRAFT_STAGE" = true ]; then
  :
elif [ -n "$VERIFICATION_FILE" ]; then
  verify_dossier
else
  REUSED_GATE="$(reusable_gate)"
fi
if [ -n "$REUSED_GATE" ]; then
  echo "finalize-pr: reusing gate $REUSED_GATE, which ran on this tree; no gate runs here" >&2
elif [ "$DRAFT_STAGE" = false ] && [ -z "$VERIFICATION_FILE" ]; then
  if [ "$GATE_MODE" = ci ]; then
    CI_GATE=true
  elif [ "$GATE_MODE" = auto ] && [ "$CI_CHECKS" != "[]" ] && [ "$DRAFT" = false ]; then
    LOAD_RATIO="$(bash "$(dirname "$TREE_FINGERPRINT")/machine-load.sh" 2>/dev/null || true)"
    if [ -n "$LOAD_RATIO" ] && awk -v ratio="$LOAD_RATIO" -v limit="$CI_LOAD" 'BEGIN { exit !(ratio > limit) }'; then
      echo "finalize-pr: machine load $LOAD_RATIO is above $CI_LOAD; the hosted checks gate this change instead of a local run" >&2
      CI_GATE=true
    fi
  fi
  if [ "$CI_GATE" = true ]; then
    if [ "$CI_CHECKS" = "[]" ]; then
      echo "finalize-pr: --gate ci needs the hosted checks that stand for the gate, in the policy's verification.ciGate.checks; nothing was written" >&2
      exit 1
    fi
    if [ "$DRAFT" = true ]; then
      echo "finalize-pr: the policy keeps proposals as drafts (publication.draft), and the hosted checks start only on a proposal ready for review; finalize with --gate local; nothing was written" >&2
      exit 1
    fi
  else
    # Which gate record already existed, so the run below can be told from it. The
    # implementer's own gate left one here, and reading that one instead would
    # compare this tree against a run that never happened in this invocation.
    LAST_GATE_BEFORE="$(gate_records | tail -1)"

    # The canonical gate, with --impacted and never per project: the projects the
    # change touched were already gated on this same tree by whoever implemented
    # it, and --impacted adds the one selection a --projects run cannot make:
    # every project the change reaches through the dependency graph. `validate`
    # also plans `validate-workspace` (manifest `alsoRuns`).
    #
    # A red run is retried exactly once, with --retry-failed, and the verdict is the
    # retry's. A failed task is recorded at the cache key of its success, so a later
    # run with unchanged inputs replays the failure instead of executing it;
    # --retry-failed re-executes only the recorded failures and keeps every other
    # cache hit. A genuinely broken tree still fails, twice, naming the same task.
    if ! "$PUTNAMI_CLI" lint,test,build,validate --impacted --enforce-coverage; then
      echo "finalize-pr: the gate failed; re-running it once with --retry-failed, which re-executes recorded failures instead of replaying them" >&2
      "$PUTNAMI_CLI" lint,test,build,validate --impacted --enforce-coverage --retry-failed
    fi

    # The tree the gate CONSUMED is read from the record that gate wrote, not
    # recomputed here: the CLI captures it before its first task runs, so it is the
    # producer's own statement about its inputs.
    GATE_RECORD="$(gate_records | tail -1)"
    if [ -z "$GATE_RECORD" ] || [ "$GATE_RECORD" = "$LAST_GATE_BEFORE" ]; then
      echo "finalize-pr: the gate recorded no session; cannot establish which tree it ran on" >&2
      exit 1
    fi
    PRE_GATE_TREE="$(jq -r '.tree.fingerprint // empty' "$GATE_RECORD")"
    if [ -z "$PRE_GATE_TREE" ]; then
      echo "finalize-pr: gate record $GATE_RECORD carries no tree fingerprint; upgrade the CLI" >&2
      exit 1
    fi

    POST_GATE_TREE="$(tree_fingerprint)"
    if [ "$PRE_GATE_TREE" != "$POST_GATE_TREE" ]; then
      echo "finalize-pr: gate changed the working tree; rerun local proof, then invoke the finalizer again" >&2
      exit 1
    fi
  fi
fi

# This helper records rather than invents behavioral evidence. It cannot
# establish that caller-supplied proof is truthful, current, or complete.

if [ "$DRAFT_STAGE" = true ]; then
  # A proposal needs a commit ahead of its base. The empty one carries the
  # title and disappears when the proposal is squashed.
  if [ "$(git rev-list --count "$REVISION_BASE..HEAD")" -eq 0 ]; then
    if [ -n "$TASK_REFERENCE_LINE" ]; then
      git commit -q --allow-empty -m "$TITLE" -m "$TASK_REFERENCE_LINE"
    else
      git commit -q --allow-empty -m "$TITLE"
    fi
  fi
elif [ -n "$(git status --porcelain)" ]; then
  if [ "${#FILES[@]}" -eq 0 ]; then
    echo "finalize-pr: dirty tree requires one --file per intended path" >&2
    exit 1
  fi
  for file in "${FILES[@]}"; do
    git add -A -- "$file"
  done

  if ! git diff --quiet || [ -n "$(git ls-files --others --exclude-standard)" ]; then
    echo "finalize-pr: unenumerated changes remain; pass every intended path with --file" >&2
    git status --short >&2
    exit 1
  fi
  git diff --cached --quiet && { echo "finalize-pr: no staged change to commit" >&2; exit 1; }
  # The gate, run here or reused, covered the working tree, and the index now
  # holds all of it. The commit moves HEAD, so the tree fingerprint changes,
  # but the committed content must not: a commit hook that rewrites a staged
  # file would publish bytes no gate saw.
  GATED_CONTENT="$(git write-tree)"
  if [ -n "$TASK_REFERENCE_LINE" ]; then
    git commit -m "$TITLE" -m "$TASK_REFERENCE_LINE"
  else
    git commit -m "$TITLE"
  fi
  if [ "$(git rev-parse 'HEAD^{tree}')" != "$GATED_CONTENT" ]; then
    echo "finalize-pr: a commit hook changed the committed content after the gate; nothing was pushed. Invoke the finalizer again: it gates the committed tree" >&2
    exit 1
  fi
fi

if [ "$(git rev-list --count "$REVISION_BASE..HEAD")" -eq 0 ]; then
  echo "finalize-pr: branch has no commit ahead of '$REVISION_BASE'" >&2
  exit 1
fi

# A resumed branch may carry commits this helper did not create: check them all
# before anything is pushed. Commits already on the base are not this change's.
while IFS= read -r sha; do
  MARKER="$(git log -1 --format=%B "$sha" | agent_marker_in)"
  if [ -n "$MARKER" ]; then
    echo "finalize-pr: commit $(git rev-parse --short "$sha") names an agent; reword it without that line, then finalize again: $MARKER" >&2
    exit 1
  fi
done < <(git rev-list --no-merges "$REVISION_BASE..HEAD")

# The host wraps the squash commit message at its own width, so a body wrapped
# by its author reaches the base with broken lines. Join the lines of each
# paragraph and list item; blank lines, list markers, fenced blocks and
# indented code keep their breaks.
unwrap_paragraphs() {
  awk '
    function flush() { if (held != "") print held; held = "" }
    /^[[:space:]]*(```|~~~)/ { flush(); print; fence = !fence; next }
    fence { print; next }
    /^[[:space:]]*$/ { flush(); print; next }
    /^[[:space:]]*([-*+]|[0-9]+[.)])[[:space:]]/ { flush(); held = $0; sub(/[[:space:]]+$/, "", held); next }
    held == "" && /^(    |\t)/ { print; next }
    {
      line = $0
      sub(/^[[:space:]]+/, "", line)
      sub(/[[:space:]]+$/, "", line)
      held = (held == "") ? line : held " " line
    }
    END { flush() }
  '
}

# The proposal body is the squash commit message: the caller's body and the
# task reference line when the policy defines one and the body lacks it.
BODY="$(unwrap_paragraphs <"$BODY_FILE")"
if [ -n "$TASK_REFERENCE_LINE" ] && ! grep -Fxq -- "$TASK_REFERENCE_LINE" "$BODY_FILE"; then
  BODY="${BODY}

${TASK_REFERENCE_LINE}"
fi
# The proposals contract carries at most 65536 bytes of body. A longer one is
# refused here, before the push, not by the provider after it.
BODY_BYTES="$(printf '%s' "$BODY" | wc -c | tr -d '[:space:]')"
if [ "$BODY_BYTES" -gt 65536 ]; then
  echo "finalize-pr: the proposal body is $BODY_BYTES bytes, and the proposals contract carries at most 65536; shorten --body-file, then finalize again. Nothing was pushed." >&2
  exit 1
fi

# The verification record is a comment on the proposal, never part of the
# commit message. It names the gate and the proof, not the files: the proposal
# diff already lists them.
GATE_DESCRIPTION=""
if [ "$DRAFT_STAGE" = false ]; then
  GATE_DESCRIPTION="lint, test, build, validate pass for ${PROJECTS[*]}, then once with --impacted"
  if [ -n "$VERIFICATION_FILE" ]; then
    GATE_DESCRIPTION="reused current evidence accepted by the execute verifier; dossier: $VERIFICATION_FILE"
  elif [ -n "$REUSED_GATE" ]; then
    # Described from the reused record, not from the run this helper would make.
    GATE_DESCRIPTION="$(jq -r --arg projects "${PROJECTS[*]}" '"\(.commands | join(", ")) pass for \($projects), then with --\(.selection.mode) in session \(.sessionId), reused on this tree"' "$REUSED_GATE")"
  fi
fi

verify_dossier
git push -u origin "$BRANCH"
HEAD_COMMIT="$(git rev-parse HEAD)"

# A draft stays a draft. The hosted checks start once the proposal is ready
# for review, so the hosted gate publishes it ready and returns it to draft
# when a check fails.
PUBLISH_DRAFT="$DRAFT"
if [ "$DRAFT_STAGE" = true ]; then
  PUBLISH_DRAFT=true
elif [ "$CI_GATE" = true ]; then
  PUBLISH_DRAFT=false
fi

verify_dossier
# The body reaches jq on standard input too: no process receives it as an
# argument.
UPSERT_REQUEST="$(printf '%s' "$BODY" | jq -cRs --arg base "$BASE" --arg head "$BRANCH" --arg commit "$HEAD_COMMIT" \
  --arg title "$TITLE" --argjson draft "$PUBLISH_DRAFT" \
  '{change: {base: $base, head: $head, headCommit: $commit}, title: $title, body: ., draft: $draft}')"
collab proposals upsert "$UPSERT_REQUEST"
require_ok "publishing the proposal for $BRANCH"
PROPOSAL_REF="$(jq -c '.result.proposal.ref' <<<"$COLLAB_ENVELOPE")"
# The assignees and labels the upsert answered with, each only when the
# provider reports it: what the read-back below must still carry.
UPSERTED_MEMBERS="$(jq -c '.result.proposal | with_entries(select(.key == "assignees" or .key == "labels"))' <<<"$COLLAB_ENVELOPE")"

# Read the proposal back from the provider rather than trusting the write's echo.
collab proposals status "$(jq -cn --argjson ref "$PROPOSAL_REF" '{ref: $ref}')"
require_ok "reading the proposal back"
[ "$(jq -r '.result.proposal.change.head' <<<"$COLLAB_ENVELOPE")" = "$BRANCH" ] ||
  { echo "finalize-pr: proposal head verification failed" >&2; exit 1; }
[ "$(jq -r '.result.proposal.change.base' <<<"$COLLAB_ENVELOPE")" = "$BASE" ] ||
  { echo "finalize-pr: proposal base verification failed" >&2; exit 1; }
PUBLISHED_COMMIT="$(jq -r '.result.proposal.change.headCommit // ""' <<<"$COLLAB_ENVELOPE")"
if [ -n "$PUBLISHED_COMMIT" ] && [ "$PUBLISHED_COMMIT" != "$HEAD_COMMIT" ] &&
  [ "${HEAD_COMMIT#"$PUBLISHED_COMMIT"}" = "$HEAD_COMMIT" ]; then
  echo "finalize-pr: proposal head commit verification failed: the provider shows $PUBLISHED_COMMIT, the pushed head is $HEAD_COMMIT" >&2
  exit 1
fi
case "$(jq -r '.result.proposal.state' <<<"$COLLAB_ENVELOPE")" in
  open|draft) ;;
  *) echo "finalize-pr: proposal state verification failed: $(jq -r '.result.proposal.state' <<<"$COLLAB_ENVELOPE")" >&2; exit 1 ;;
esac
# Assignees and labels, including those the provider applies from its own
# settings (an author assignment, labels inherited from the task): the
# proposal read back must carry every one the upsert answered with. Absent
# from both answers means the provider does not report the member, which is
# said, never taken as verified. Both documents reach jq on standard input.
for member in assignees labels; do
  VERDICT="$(printf '%s\n%s\n' "$UPSERTED_MEMBERS" "$COLLAB_ENVELOPE" | jq -rs --arg member "$member" '
    .[0] as $upserted | .[1].result.proposal as $read |
    (($upserted[$member] // []) - ($read[$member] // [])) as $missing |
    if ($read | has($member) | not) then
      (if ($upserted | has($member)) then "dropped" else "unreported" end)
    elif ($missing | length) > 0 then "missing " + ($missing | join(", "))
    elif ($read[$member] | length) == 0 then "ok none"
    else "ok " + ($read[$member] | join(", ")) end')"
  case "$VERDICT" in
    unreported)
      echo "finalize-pr: the proposals provider does not report $member; they are not verified" >&2 ;;
    dropped)
      echo "finalize-pr: proposal $member verification failed: the upsert answer reports them, and the proposal read back does not" >&2
      exit 1 ;;
    missing\ *)
      echo "finalize-pr: proposal $member verification failed: the upsert answered with ${VERDICT#missing }, and the proposal read back does not carry it" >&2
      exit 1 ;;
    *)
      echo "finalize-pr: proposal $member: ${VERDICT#ok }" >&2 ;;
  esac
done
PROPOSAL_URL="$(jq -r '.result.proposal.url // ""' <<<"$COLLAB_ENVELOPE")"
echo "finalize-pr: proposal checks: $(jq -r '.result.checks.state' <<<"$COLLAB_ENVELOPE")" >&2

# wait_for_hosted_checks reads the proposal until every check the policy names
# passes or one fails on the pushed commit, or the policy's timeout ends. It
# leaves passing, failing, unsupported or timeout, with the named checks, in
# CI_VERDICT. A named check that is not reported yet is pending; a check
# re-run on the same commit passes once one of its runs passes.
CI_VERDICT=""
wait_for_hosted_checks() {
  local deadline=$(($(date +%s) + CI_TIMEOUT_MINUTES * 60))
  while :; do
    collab proposals status "$(jq -cn --argjson ref "$PROPOSAL_REF" '{ref: $ref}')"
    require_ok "reading the hosted checks"
    CI_VERDICT="$(jq -r --arg head "$HEAD_COMMIT" --argjson names "$CI_CHECKS" '
      .result.checks as $checks |
      ($checks.commit // "") as $commit |
      if $checks.state == "unsupported" then "unsupported"
      elif $commit != "" and ($head | startswith($commit) | not) and ($commit | startswith($head) | not) then "pending"
      else
        [$names[] as $name | [$checks.items[]? | select(.name == $name)] as $runs |
          (if any($runs[]; .state == "passing") then "passing"
           elif any($runs[]; .state == "pending") then "pending"
           elif any($runs[]; .state == "failing") then "failing"
           else "pending" end) as $state |
          {name: $name, state: $state, url: ([$runs[] | select(.state == $state) | .url // empty][0] // "")}] as $named |
        if any($named[]; .state == "failing") then
          "failing " + ([$named[] | select(.state == "failing") | "\(.name) \(.url)"] | join("; "))
        elif all($named[]; .state == "passing") then
          "passing " + ([$named[] | "\(.name) \(.url)"] | join("; "))
        else "pending" end
      end' <<<"$COLLAB_ENVELOPE")"
    case "$CI_VERDICT" in
      passing* | failing* | unsupported) return 0 ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      CI_VERDICT=timeout
      return 0
    fi
    sleep "$CI_POLL_SECONDS"
  done
}

# return_to_draft republishes the proposal as a draft, so later pushes start
# no hosted run until it is ready again.
return_to_draft() {
  collab proposals upsert "$(jq -c '.draft = true' <<<"$UPSERT_REQUEST")"
  require_ok "returning the proposal to draft"
}

if [ "$CI_GATE" = true ]; then
  echo "finalize-pr: waiting for the hosted checks $(jq -r 'join(", ")' <<<"$CI_CHECKS") on $HEAD_COMMIT, at most $CI_TIMEOUT_MINUTES minutes" >&2
  wait_for_hosted_checks
  case "$CI_VERDICT" in
    passing*)
      GATE_DESCRIPTION="hosted checks passed on $HEAD_COMMIT: ${CI_VERDICT#passing }"
      ;;
    failing*)
      return_to_draft
      echo "finalize-pr: hosted checks failed on $HEAD_COMMIT: ${CI_VERDICT#failing }. The proposal is a draft again; fix the change, then finalize again" >&2
      exit 1
      ;;
    unsupported)
      return_to_draft
      echo "finalize-pr: the proposals provider runs no hosted checks; finalize with --gate local. The proposal is a draft again" >&2
      exit 1
      ;;
    *)
      echo "finalize-pr: the hosted checks are still pending on $HEAD_COMMIT after $CI_TIMEOUT_MINUTES minutes; they keep running. Invoke the finalizer again to keep waiting" >&2
      exit 1
      ;;
  esac
fi

if [ "$DRAFT_STAGE" = false ]; then
  VERIFICATION="Verification of $HEAD_COMMIT

- ordinary gate: $GATE_DESCRIPTION
- local proof ($PROOF_STATUS): $PROOF"
  if [ "$REVIEW_SUPPORTED" = true ]; then
    # One comment per head commit and record: a resumed run publishes nothing new.
    REVIEW_KEY="verification:$HEAD_COMMIT:$(printf '%s' "$VERIFICATION" | git hash-object --stdin | cut -c1-12)"
    REVIEW_REQUEST="$(printf '%s' "$VERIFICATION" | jq -cRs --argjson ref "$PROPOSAL_REF" --arg commit "$HEAD_COMMIT" --arg key "$REVIEW_KEY" \
      '{ref: $ref, verdict: "comment", body: ., commit: $commit, idempotencyKey: $key}')"
    collab proposals review "$REVIEW_REQUEST"
    require_ok "publishing the verification comment"
    echo "finalize-pr: verification comment published on the proposal" >&2
  else
    echo "finalize-pr: the proposals provider offers no review, so the verification record stays here:" >&2
    printf '%s\n' "$VERIFICATION" >&2
  fi
fi

# The transition is sent even when the task already reads as the delivered
# state. A provider can represent one semantic state several ways (a tracker
# label that means "blocked" for another reason); only the provider knows
# whether its representation still has to change, and it writes nothing when
# it does not.
if [ -n "$TASK_REF" ] && [ "$DRAFT_STAGE" = false ]; then
  collab tasks get "$(jq -cn --argjson ref "$TASK_REF" '{ref: $ref}')"
  require_ok "reading task $TASK_ID"
  REVISION="$(jq -r '.result.task.revision' <<<"$COLLAB_ENVELOPE")"
  REASON="Proposal ${PROPOSAL_URL:-$(jq -r '.id' <<<"$PROPOSAL_REF")} published for review"
  REQUEST="$(jq -cn --argjson ref "$TASK_REF" --arg state "$DELIVERED_STATE" --arg reason "${REASON:0:256}" \
    '{ref: $ref, state: $state, reason: $reason}')"
  case "$TASK_PRECONDITIONS" in
    atomic|checked) REQUEST="$(jq -c --arg revision "$REVISION" '. + {expectedRevision: $revision}' <<<"$REQUEST")" ;;
  esac
  collab tasks transition "$REQUEST"
  require_ok "moving task $TASK_ID to $DELIVERED_STATE"
  collab tasks get "$(jq -cn --argjson ref "$TASK_REF" '{ref: $ref}')"
  require_ok "reading task $TASK_ID back"
  STATE="$(jq -r '.result.task.state' <<<"$COLLAB_ENVELOPE")"
  [ "$STATE" = "$DELIVERED_STATE" ] || {
    echo "finalize-pr: task state verification failed: $STATE, expected $DELIVERED_STATE" >&2
    exit 1
  }
  echo "finalize-pr: task $TASK_ID: $STATE ($(jq -r '.result.task.providerState // .result.task.state' <<<"$COLLAB_ENVELOPE"))" >&2
fi

printf 'PROPOSAL_REF=%s\n' "$PROPOSAL_REF"
if [ -n "$PROPOSAL_URL" ]; then
  printf 'PROPOSAL_URL=%s\n' "$PROPOSAL_URL"
fi
