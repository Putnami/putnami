#!/usr/bin/env bash
# Publishes a release of the GitLab CI/CD catalog component into a clone of
# the gitlab.com project putnami/observe.
#
# Usage: release.sh <X.Y.Z> <clone> [<source revision>]
#
# The clone's checked-out branch receives one commit whose tree is
# templates/observe.yml and README.md from this directory, project.gitlab-ci.yml
# as .gitlab-ci.yml, and LICENSE.md from the repository root, tagged X.Y.Z with
# no v, as the catalog reads semantic versions. One atomic push moves the
# branch and the tag on the clone's origin, or neither; the tag pipeline of the
# project then publishes the version to the catalog. A version that is not
# X.Y.Z, or an X.Y.Z tag that origin already holds, stops before any write.
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
tag="$version"

component_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
licence="$component_dir/../../../LICENSE.md"
[[ -f "$licence" ]] || fail "no LICENSE.md at the repository root"

cd "$clone"
branch="$(git symbolic-ref --short HEAD)" || fail "$clone has no checked-out branch"
git fetch --quiet --force --tags origin
if git rev-parse --quiet --verify "refs/tags/$tag" >/dev/null; then
  fail "tag $tag already exists"
fi

git rm -r --quiet --ignore-unmatch -- .
mkdir -p templates
cp "$component_dir/templates/observe.yml" templates/observe.yml
cp "$component_dir/README.md" README.md
cp "$component_dir/project.gitlab-ci.yml" .gitlab-ci.yml
cp "$licence" LICENSE.md
git add -- templates/observe.yml README.md .gitlab-ci.yml LICENSE.md

message="Release $tag"
if [[ -n "$source_revision" ]]; then
  message+=$'\n\n'"Source: $source_revision"
fi
git commit --quiet --allow-empty -m "$message"
git tag "$tag"

if ! git push --quiet --atomic origin "HEAD:refs/heads/$branch" "refs/tags/$tag"; then
  # Nothing moved on origin: drop the local tag so a retry is not refused.
  git tag --delete "$tag" >/dev/null
  fail "origin refused the push; nothing was released"
fi
echo "release: $tag points at $(git rev-parse HEAD) on $branch"
