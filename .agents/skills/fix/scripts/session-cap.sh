#!/usr/bin/env bash
# Count the orchestrator's own tool calls per agent session and tell the model,
# once, when the session has grown long. It never blocks a call.
#
# This is a Claude Code PreToolUse hook. The harness pipes one JSON object on
# stdin for every tool call; the hook keys a counter on its `session_id`, and:
#   - a call made inside a subagent carries `agent_id`: it is not counted. A
#     subagent starts from an empty context and is the cheapest mode there is;
#     counting its calls against the orchestrator stopped a 12-worker fan-out
#     in five minutes while the orchestrator's own context sat at 140k tokens;
#   - at SESSION_LONG own calls it lets the call through and hands the model a
#     warning as additional context: save the checkpoint now, so that a
#     compaction or a relaunch has something to consume;
#   - it exits 0 always. The cost this sensor watches, context replayed per
#     turn, is bounded by the harness's own auto-compaction
#     (CLAUDE_CODE_AUTO_COMPACT_WINDOW): a long session is summarized and goes
#     on, instead of being blocked and relaunched from a checkpoint.
#
# The sensor counts requests, never wall-clock or tokens: a hook cannot observe
# either. Codex has no hook; the guidance is stated in the skill and compliance
# is read after the fact from the session record.
#
# Wire it in .claude/settings.json (a workspace choice, not the artifact's):
#   "hooks": { "PreToolUse": [ { "hooks": [ { "type": "command",
#     "command": "bash \"$CLAUDE_PROJECT_DIR/.agents/skills/fix/scripts/session-cap.sh\"" } ] } ] }
set -euo pipefail

SESSION_LONG=250

# One state directory that does not depend on the working directory: a session
# that changes cwd keeps one count. A hook that failed here would block every
# tool call, so the directory is always creatable.
STATE_DIR="${TMPDIR:-/tmp}/putnami-agent-sessions"

# Under Git for Windows a native jq.exe ends every output line with CRLF, and
# the CR left in a captured value fails every comparison; --binary keeps LF.
case "${OSTYPE:-}" in
  msys* | cygwin*) jq() { command jq --binary "$@"; } ;;
esac

payload="$(cat)"
session_id="$(printf '%s' "$payload" | jq -r '.session_id // empty' 2>/dev/null || true)"
# A payload without a session id is not this hook's to judge: it neither counts
# nor warns, because a counter keyed on nothing would mix unrelated sessions.
[ -n "$session_id" ] || exit 0
case "$session_id" in
  *[!A-Za-z0-9._-]*) exit 0 ;;
esac
# A subagent's call is not the orchestrator's: let it through uncounted.
agent_id="$(printf '%s' "$payload" | jq -r '.agent_id // empty' 2>/dev/null || true)"
[ -z "$agent_id" ] || exit 0

mkdir -p "$STATE_DIR"
counter="$STATE_DIR/$session_id.count"
count=0
if [ -f "$counter" ]; then
  count="$(tr -dc '0-9' <"$counter")"
  count="${count:-0}"
fi
count=$((count + 1))
# Parallel tool calls run their hooks concurrently; an overlapping increment
# undercounts by one and the warning fires one call later. That is the
# precision this sensor needs, and a lock would be a second thing that can
# wedge a call.
printf '%s\n' "$count" >"$counter.tmp.$$"
mv -f "$counter.tmp.$$" "$counter"

if [ "$count" -eq "$SESSION_LONG" ]; then
  jq -cn --arg message "session sensor: $count tool calls by this session — save the mission checkpoint now (\`putnami memory checkpoint\` when the workspace binds memory, otherwise the run record; what the next session consumes stays <= 2 KB) so a compaction or a relaunch has something to consume; the session is not stopped" '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "allow",
      additionalContext: $message
    }
  }'
fi
exit 0
