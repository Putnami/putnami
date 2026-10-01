#!/usr/bin/env bash
# Print one digest identifying a worktree's exact state: the commit it sits on,
# the bytes at every tracked path that differs from that commit (recursing
# through submodules), and the content of every untracked, non-ignored file.
#
# It exists because "the same files are dirty" is not "the same bytes are on
# disk". `git status --porcelain` reports paths and status codes, so a worker
# that gates a tree and then edits one of the files it already listed leaves the
# status output identical: a path-only check accepts a gate that proved nothing
# about the content now on disk. Comparing this digest does prove it.
#
# One definition, one implementation, every caller. The digest is computed by
# `putnami tree fingerprint`, and this script only finds the CLI and forwards
# the answer. It used to carry its own `git hash-object` pipeline, which made
# two implementations the moment the CLI started recording the same digest in
# every session (`session.json`'s `tree` block): two agents comparing
# fingerprints produced by two implementations compare nothing. There is
# deliberately no shell fallback — a second calculation here is exactly the
# defect this shape removes, and a missing CLI is reported rather than
# approximated.
#
# The digest covers HEAD as well as the delta. A delta is only meaningful
# against the commit it was taken from, and folding HEAD in means a moved HEAD
# shows up as a different tree instead of cancelling out against a
# coincidentally identical diff. The exact byte stream that is hashed is
# specified once, in protocols/cli/doc/02-result-v2.md § Gated tree fingerprint.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  bash tree-fingerprint.sh

Prints one line: the digest of the current worktree's HEAD plus its uncommitted
state. It reads the repository and writes nothing — no session record, no
workspace state. Run it from anywhere inside the worktree; the CLI resolves the
repository root itself, so two callers in different directories agree, and no
workspace (no putnami.json) is required.
USAGE
}

case "${1:-}" in
  --help | -h)
    usage
    exit 0
    ;;
  "") ;;
  *)
    echo "tree-fingerprint: unknown argument '$1'" >&2
    usage >&2
    exit 2
    ;;
esac

# The CLI is resolved relative to THIS SCRIPT, never relative to the tree being
# fingerprinted: the two differ every time an agent fingerprints a repository
# that is not the workspace the skill was installed into, which is exactly what
# this script's own test does. `./putnamiw` is the entrypoint wherever it exists
# (a source workspace pins its CLI to the tree, so a globally installed binary
# is the wrong one there); elsewhere the installed `putnami` is it.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

if [ -x "$WORKSPACE_ROOT/putnamiw" ]; then
  CLI=("$WORKSPACE_ROOT/putnamiw")
elif command -v putnami >/dev/null 2>&1; then
  CLI=(putnami)
else
  echo "tree-fingerprint: no Putnami CLI found — expected $WORKSPACE_ROOT/putnamiw or putnami on PATH" >&2
  exit 1
fi

# The digest is taken against the tree the CALLER is in, so the CLI runs with
# this shell's working directory untouched.
"${CLI[@]}" tree fingerprint
