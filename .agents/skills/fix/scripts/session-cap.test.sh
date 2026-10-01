#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
HOOK="$ROOT/.agents/skills/fix/scripts/session-cap.sh"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

fail() {
  echo "session-cap test: $*" >&2
  exit 1
}

# The threshold under test is the script's own constant: a test that hardcoded
# 250 would keep passing after someone moved it.
LONG="$(sed -n 's/^SESSION_LONG=\([0-9]*\)$/\1/p' "$HOOK")"
[ -n "$LONG" ] || fail "the threshold is not one constant at the top of the script"

# The counters land under TMPDIR, so the test points TMPDIR at its own
# directory and nothing touches the real one.
export TMPDIR="$TEST_DIR/tmp"
STATE="$TMPDIR/putnami-agent-sessions"
WORK="$TEST_DIR/work"
mkdir -p "$WORK"
cd "$WORK"

# call <payload-json> <stdout-file> <stderr-file>; prints the exit status.
call() {
  local status=0
  printf '%s' "$1" | bash "$HOOK" >"$2" 2>"$3" || status=$?
  echo "$status"
}
own() { printf '{"session_id":"%s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"true"}}' "$1"; }
sub() { printf '{"session_id":"%s","agent_id":"a1","agent_type":"Explore","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"true"}}' "$1"; }
count_of() { tr -d '[:space:]' <"$STATE/$1.count"; }

# Every call before the warning passes silently: nothing on stdout, nothing on
# stderr, exit 0. A hook that printed on every call would flood the transcript.
for ((n = 1; n < LONG; n++)); do
  status="$(call "$(own one)" "$TEST_DIR/out" "$TEST_DIR/err")"
  [ "$status" -eq 0 ] || fail "call $n exited $status, want 0"
  [ ! -s "$TEST_DIR/out" ] || fail "call $n printed on stdout before the warning: $(cat "$TEST_DIR/out")"
  [ ! -s "$TEST_DIR/err" ] || fail "call $n printed on stderr before the warning: $(cat "$TEST_DIR/err")"
done
[ "$(count_of one)" = "$((LONG - 1))" ] || fail "the counter does not persist under TMPDIR/putnami-agent-sessions"

# A subagent's call is let through and not counted: it carries agent_id, it
# starts from an empty context, and counting it once stopped a 12-worker
# fan-out in five minutes.
status="$(call "$(sub one)" "$TEST_DIR/sub.out" "$TEST_DIR/sub.err")"
[ "$status" -eq 0 ] || fail "a subagent call exited $status, want 0"
[ ! -s "$TEST_DIR/sub.out" ] || fail "a subagent call printed on stdout: $(cat "$TEST_DIR/sub.out")"
[ "$(count_of one)" = "$((LONG - 1))" ] || fail "a subagent call was counted against the session"

# The LONG-th own call passes and warns on stdout, as the JSON the harness
# reads so the message reaches the model as context rather than the transcript log.
status="$(call "$(own one)" "$TEST_DIR/warn.out" "$TEST_DIR/warn.err")"
[ "$status" -eq 0 ] || fail "call $LONG exited $status, want 0"
jq -e '.hookSpecificOutput.permissionDecision == "allow"' "$TEST_DIR/warn.out" >/dev/null ||
  fail "call $LONG did not allow the call: $(cat "$TEST_DIR/warn.out")"
warning="$(jq -r '.hookSpecificOutput.additionalContext' "$TEST_DIR/warn.out")"
printf '%s' "$warning" | grep -Fq "save the mission checkpoint now" ||
  fail "call $LONG did not tell the model to save the checkpoint: $warning"
printf '%s' "$warning" | grep -Fq "putnami memory checkpoint" ||
  fail "call $LONG did not name the memory checkpoint: $warning"
printf '%s' "$warning" | grep -Fq "2 KB" || fail "call $LONG did not bound what the next session consumes"
printf '%s' "$warning" | grep -Fq "not stopped" || fail "call $LONG did not say the session goes on"
# The hook names the contract, never where a memory backend stores a mission.
if printf '%s' "$warning" | grep -Eq "mission[s]/|\.md\b"; then
  fail "call $LONG names a storage location: $warning"
fi

# Past the warning the session goes on: every call passes silently, the count
# keeps rising and nothing is ever blocked. Auto-compaction bounds the context,
# not this hook.
for ((n = LONG + 1; n <= LONG + 5; n++)); do
  status="$(call "$(own one)" "$TEST_DIR/out" "$TEST_DIR/err")"
  [ "$status" -eq 0 ] || fail "call $n exited $status, want 0: the hook must never block"
  [ ! -s "$TEST_DIR/out" ] || fail "call $n printed on stdout after the warning"
  [ ! -s "$TEST_DIR/err" ] || fail "call $n printed on stderr after the warning"
done
[ "$(count_of one)" = "$((LONG + 5))" ] || fail "the counter stopped at the warning"

# The count follows the session, not the working directory: a call from
# another cwd continues the same counter.
ELSEWHERE="$TEST_DIR/elsewhere"
mkdir -p "$ELSEWHERE"
status=0
(cd "$ELSEWHERE" && printf '%s' "$(own one)" | bash "$HOOK") >/dev/null 2>&1 || status=$?
[ "$status" -eq 0 ] || fail "from another cwd the hook exited $status"
[ "$(count_of one)" = "$((LONG + 6))" ] || fail "a cwd change split the counter"

# A second session id starts at zero: the count is per session.
status="$(call "$(own two)" "$TEST_DIR/two.out" "$TEST_DIR/two.err")"
[ "$status" -eq 0 ] || fail "a fresh session exited $status on its first call"
[ ! -s "$TEST_DIR/two.out" ] || fail "a fresh session was warned on its first call"
[ "$(count_of two)" = "1" ] || fail "a fresh session did not start at zero"

# A payload with no session id is let through and counted nowhere: there is no
# session to watch, and a shared counter would mix unrelated sessions.
status=0
printf '{"hook_event_name":"PreToolUse"}' | bash "$HOOK" >"$TEST_DIR/none.out" 2>"$TEST_DIR/none.err" || status=$?
[ "$status" -eq 0 ] || fail "a payload without a session id exited $status"
[ "$(ls "$STATE" | wc -l | tr -d ' ')" = "2" ] || fail "a payload without a session id created a counter"

# A session id that could escape the state directory is ignored the same way.
status=0
printf '{"session_id":"../escape"}' | bash "$HOOK" >/dev/null 2>&1 || status=$?
[ "$status" -eq 0 ] || fail "a hostile session id exited $status"
[ ! -e "$TMPDIR/escape.count" ] && [ ! -e "$STATE/../escape.count" ] ||
  fail "a hostile session id wrote outside the state directory"

echo "session-cap test: ok"
