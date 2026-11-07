#!/usr/bin/env bash
# Publishes a release of the observe action into a clone of
# Putnami/observe-action.
#
# Usage: release.sh <X.Y.Z> <clone> [<source revision>]
#
# The clone's checked-out branch receives one commit whose tree is
# action.yml, observe.sh and README.md from this directory and LICENSE.md
# from the repository root, tagged vX.Y.Z, and the vX tag moves to it. One
# atomic push moves the branch and both tags on the clone's origin, or none of
# them. A version that is not X.Y.Z, or a vX.Y.Z tag that origin already holds,
# stops before any write.
set -euo pipefail

fail() {
  echo "release: $1" >&2
  exit 1
}

[[ $# -ge 2 && $# -le 3 ]] || fail "usage: release.sh <X.Y.Z> <clone> [<source revision>]"
version="$1"
clone="$2"
source_revision="${3:-}"

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
  || fail "version \"$version\" is not X.Y.Z"
tag="v$version"
major="v${BASH_REMATCH[1]}"

action_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
licence="$action_dir/../../../LICENSE.md"
[[ -f "$licence" ]] || fail "no LICENSE.md at the repository root"

cd "$clone"
branch="$(git symbolic-ref --short HEAD)" || fail "$clone has no checked-out branch"
git fetch --quiet --force --tags origin
if git rev-parse --quiet --verify "refs/tags/$tag" >/dev/null; then
  fail "tag $tag already exists"
fi

git rm -r --quiet --ignore-unmatch -- .
cp "$action_dir/action.yml" "$action_dir/observe.sh" "$action_dir/README.md" .
cp "$licence" LICENSE.md
chmod +x observe.sh
git add -- action.yml observe.sh README.md LICENSE.md

message="Release $tag"
if [[ -n "$source_revision" ]]; then
  message+=$'\n\n'"Source: $source_revision"
fi
git commit --quiet --allow-empty -m "$message"
git tag "$tag"
git tag --force "$major"

git push --quiet --atomic origin "HEAD:refs/heads/$branch" "refs/tags/$tag" "+refs/tags/$major"
echo "release: $tag and $major point at $(git rev-parse HEAD) on $branch"
