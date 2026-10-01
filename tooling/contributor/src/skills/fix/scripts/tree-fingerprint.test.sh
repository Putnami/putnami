#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
FINGERPRINT="$ROOT/.agents/skills/fix/scripts/tree-fingerprint.sh"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

WORK="$TEST_DIR/work"
git init -b main "$WORK" >/dev/null
git -C "$WORK" config user.name "Skill Test"
git -C "$WORK" config user.email "skill-test@example.com"
mkdir -p "$WORK/nested"
printf 'initial\n' >"$WORK/tracked.txt"
printf 'nested\n' >"$WORK/nested/deep.txt"
git -C "$WORK" add tracked.txt nested/deep.txt
git -C "$WORK" commit -q -m "initial"

fingerprint() {
  (cd "${1:-$WORK}" && bash "$FINGERPRINT")
}

clean="$(fingerprint)"
[ -n "$clean" ]

# One implementation, not two. The script resolves the CLI and forwards its
# answer; a script that recomputed the digest itself would be the second
# implementation the whole design exists to delete, and two agents comparing
# fingerprints produced by two implementations compare nothing. This is the
# check that keeps the two in lock-step — note it runs the CLI in a repository
# that is NOT a workspace, which the command must also survive.
if [ -x "$ROOT/putnamiw" ]; then
  CLI=("$ROOT/putnamiw")
else
  CLI=(putnami)
fi
cli_direct="$(cd "$WORK" && "${CLI[@]}" tree fingerprint)"
if [ "$cli_direct" != "$clean" ]; then
  echo "tree-fingerprint test: the script and the CLI printed different digests" >&2
  echo "  script: $clean" >&2
  echo "  cli:    $cli_direct" >&2
  exit 1
fi
# The script prints ONE line and nothing else: its callers capture it into a
# variable and compare it as a string, so a label or a trailing line would have
# to be stripped by every caller.
[ "$(fingerprint | wc -l | tr -d ' ')" = "1" ]

# Determinism: reading the same tree twice gives the same answer, and reading it
# from a subdirectory gives that same answer too. Two agents run this script from
# wherever they happen to be, so a cwd-dependent digest would be useless.
[ "$(fingerprint)" = "$clean" ]
[ "$(fingerprint "$WORK/nested")" = "$clean" ]

# The defect this script exists for: a tracked file that is ALREADY dirty gets
# new content. The `git status` output is byte-for-byte identical across the two
# states, so a path-only check cannot tell them apart.
printf 'gated\n' >"$WORK/tracked.txt"
gated="$(fingerprint)"
status_gated="$(git -C "$WORK" status --porcelain)"
[ "$gated" != "$clean" ]

printf 'edited after the gate\n' >"$WORK/tracked.txt"
edited="$(fingerprint)"
status_edited="$(git -C "$WORK" status --porcelain)"
[ "$status_gated" = "$status_edited" ] || {
  echo "tree-fingerprint test: the scenario is wrong, git status already differs" >&2
  exit 1
}
if [ "$gated" = "$edited" ]; then
  echo "tree-fingerprint test: content change behind an unchanged status was not detected" >&2
  exit 1
fi

# Restoring the exact bytes restores the exact digest: the check accepts an
# unchanged tree rather than merely rejecting everything.
printf 'gated\n' >"$WORK/tracked.txt"
[ "$(fingerprint)" = "$gated" ]

# Untracked content counts. Same path, different bytes, different digest.
printf 'one\n' >"$WORK/untracked.txt"
untracked_one="$(fingerprint)"
[ "$untracked_one" != "$gated" ]
printf 'two\n' >"$WORK/untracked.txt"
[ "$(fingerprint)" != "$untracked_one" ]
rm "$WORK/untracked.txt"
[ "$(fingerprint)" = "$gated" ]

# An untracked NESTED REPOSITORY is the one entry git refuses to hash: `ls-files
# --others` collapses it to "<path>/", and the `git hash-object` pipeline this
# script used to carry aborted on it with "Unable to hash". It must answer.
git init -b main "$WORK/vendored" >/dev/null
with_nested="$(fingerprint)"
[ -n "$with_nested" ]
[ "$with_nested" != "$gated" ]
rm -rf "$WORK/vendored"
[ "$(fingerprint)" = "$gated" ]

# A TRACKED SUBMODULE's content counts, and keeps counting once it is dirty.
# Version 1 hashed `git diff HEAD`, which renders a dirty submodule as
# "Subproject commit <sha>-dirty" whatever is inside it: an agent could gate the
# tree and then rewrite a submodule's source without moving the fingerprint the
# finalizer compares. This runs in its own pair of repositories so it cannot
# perturb the digests $WORK is asserted against above and below. Local file
# transports need protocol.file.allow.
SUPER="$TEST_DIR/super"
CHILD="$TEST_DIR/child"
for repo in "$CHILD" "$SUPER"; do
  git init -b main "$repo" >/dev/null
  git -C "$repo" config user.name "Skill Test"
  git -C "$repo" config user.email "skill-test@example.com"
done
printf 'v1\n' >"$CHILD/source.txt"
git -C "$CHILD" add source.txt
git -C "$CHILD" commit -q -m "source"
printf 'root\n' >"$SUPER/root.txt"
git -C "$SUPER" add root.txt
git -C "$SUPER" commit -q -m "root"
git -C "$SUPER" -c protocol.file.allow=always submodule add -q "$CHILD" vendored
git -C "$SUPER" commit -q -m "vendor the child"

sub_clean="$(fingerprint "$SUPER")"
printf 'v2\n' >"$SUPER/vendored/source.txt"
sub_dirty_one="$(fingerprint "$SUPER")"
[ "$sub_dirty_one" != "$sub_clean" ] || {
  echo "tree-fingerprint test: dirtying a submodule did not move the digest" >&2
  exit 1
}

# The defect: the submodule is ALREADY dirty and its content changes again.
printf 'v3-entirely-different\n' >"$SUPER/vendored/source.txt"
if [ "$(fingerprint "$SUPER")" = "$sub_dirty_one" ]; then
  echo "tree-fingerprint test: content changed inside an already-dirty submodule without moving the digest" >&2
  exit 1
fi
printf 'v2\n' >"$SUPER/vendored/source.txt"
[ "$(fingerprint "$SUPER")" = "$sub_dirty_one" ]
# The CLI and this script still agree once submodules are in play.
[ "$(cd "$SUPER" && "${CLI[@]}" tree fingerprint)" = "$sub_dirty_one" ]

# Settings that change only how git DISPLAYS a diff must not move the digest:
# two agents comparing a join key cannot be allowed to disagree over a config.
# Each of these moved the version-1 digest of an unchanged tree.
for setting in core.abbrev=4 core.abbrev=20 diff.mnemonicPrefix=true diff.noprefix=true diff.srcPrefix=X/ diff.context=7 diff.ignoreSubmodules=all; do
  git -C "$SUPER" config "${setting%%=*}" "${setting#*=}"
  if [ "$(fingerprint "$SUPER")" != "$sub_dirty_one" ]; then
    echo "tree-fingerprint test: $setting moved the digest" >&2
    exit 1
  fi
  git -C "$SUPER" config --unset "${setting%%=*}"
done

# An ignored file is not part of the tree a gate reasons about.
printf 'ignored.txt\n' >"$WORK/.gitignore"
git -C "$WORK" add .gitignore
git -C "$WORK" commit -q -m "ignore"
with_ignore_rule="$(fingerprint)"
printf 'noise\n' >"$WORK/ignored.txt"
[ "$(fingerprint)" = "$with_ignore_rule" ]
rm "$WORK/ignored.txt"

# A moved HEAD is a different tree even when the uncommitted delta is empty.
git -C "$WORK" checkout -q -- tracked.txt
committed_head="$(fingerprint)"
printf 'landed\n' >"$WORK/tracked.txt"
git -C "$WORK" commit -q -am "land the change"
git -C "$WORK" checkout -q -- .
if [ "$(fingerprint)" = "$committed_head" ]; then
  echo "tree-fingerprint test: a moved HEAD produced the same digest" >&2
  exit 1
fi

# A repository with no commit says so instead of printing a digest.
EMPTY="$TEST_DIR/empty"
git init -b main "$EMPTY" >/dev/null
if (cd "$EMPTY" && bash "$FINGERPRINT") >"$TEST_DIR/empty.out" 2>"$TEST_DIR/empty.err"; then
  echo "tree-fingerprint test: expected a failure on a repository with no commit" >&2
  exit 1
fi
grep -Fq "has no commit" "$TEST_DIR/empty.err"

if (cd "$WORK" && bash "$FINGERPRINT" --unexpected) >"$TEST_DIR/arg.out" 2>"$TEST_DIR/arg.err"; then
  echo "tree-fingerprint test: expected an unknown-argument failure" >&2
  exit 1
fi
grep -Fq "unknown argument" "$TEST_DIR/arg.err"

echo "tree-fingerprint test: ok"
