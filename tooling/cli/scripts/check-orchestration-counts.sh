#!/usr/bin/env bash
# Checks a run's orchestration report against the kernel and a git shim.
#
#   tooling/cli/scripts/check-orchestration-counts.sh lint,test --projects /protocols/runtime --no-cache
#
# The arguments are passed to ./putnamiw. The run executes under
# `strace -f`, with a git shim first on PATH that logs each call with the pid
# of its caller and execs the real git. The script then compares the
# processes the CLI process and its job processes started, and the git calls
# among them (the starts whose program is git), as strace saw them, with the
# run's run.orchestration (tooling/cli/doc/12-profiling-and-telemetry.md). The
# shim log only bounds the engine's git calls from above. It prints
# one JSON document; it fails only when the run cannot be traced.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
work="$(mktemp -d)"
# ORCHESTRATION_CHECK_KEEP=1 keeps the trace and the shim log.
if [[ "${ORCHESTRATION_CHECK_KEEP:-}" == "1" ]]; then
  echo "keeping $work" >&2
else
  trap 'rm -rf "$work"' EXIT
fi

real_git="$(command -v git)"
mkdir -p "$work/bin"
cat >"$work/bin/git" <<SHIM
#!/bin/sh
printf '%s %s %s\n' "\$PPID" "\$\$" "\$*" >>"$work/git.log"
exec "$real_git" "\$@"
SHIM
chmod +x "$work/bin/git"
: >"$work/git.log"

cd "$root"
# Build the CLI outside the traced run, so the trace holds the run alone.
./putnamiw --version >/dev/null
sessions() { ls -1 .putnami/sessions 2>/dev/null | grep -E '^[0-9]{8}-' | sort; }
before="$(sessions | tail -n 1 || true)"
PATH="$work/bin:$PATH" strace -f -qq -ttt -v -s 4096 -e trace=execve,clone,clone3,fork,vfork -o "$work/strace.log" \
  ./putnamiw "$@" || true

session=""
for id in $(sessions); do
  if [[ "$id" > "$before" ]] && ! jq -e '.parentSessionId' ".putnami/sessions/$id/session.json" >/dev/null 2>&1; then
    session="$id"
  fi
done
if [[ -z "$session" ]]; then
  echo "no top-level session was recorded" >&2
  exit 1
fi
python3 "$root/tooling/cli/scripts/orchestration_counts.py" "$work/strace.log" "$work/git.log" \
  ".putnami/sessions/$session/session.json" | jq --arg session "$session" '. + {session: $session}'
