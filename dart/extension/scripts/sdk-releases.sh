#!/bin/sh
# Writes sdkreleases.json, the Dart and Flutter SDK releases @putnami/dart
# installs: the newest patch of each stable Dart minor from 2.19, with the
# SHA-256 the dart-archive publishes for each platform's archive, and the
# newest stable Flutter release of each minor from 3.10 on each platform, with
# the SHA-256 its release manifest states. A pin moves only when a release of
# the extension ships a regenerated file (ADR 0003 of @putnami/dart).
#
#   dart/extension/scripts/sdk-releases.sh
#
# It needs curl and jq.
set -eu

out="$(cd "$(dirname "$0")/.." && pwd)/sdkreleases.json"
archive="https://storage.googleapis.com/dart-archive/channels/stable/release"
list="https://storage.googleapis.com/storage/v1/b/dart-archive/o?prefix=channels/stable/release/&delimiter=/"
flutter="https://storage.googleapis.com/flutter_infra_release/releases"
platforms="linux-x64 linux-arm64 macos-x64 macos-arm64 windows-x64 windows-arm64"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Every stable Dart release, page by page.
token=""
: > "$tmp/versions"
while :; do
  url="$list"
  [ -n "$token" ] && url="$url&pageToken=$(printf '%s' "$token" | jq -sRr @uri)"
  curl -fsSL "$url" > "$tmp/page"
  jq -r '.prefixes[] | rtrimstr("/") | split("/") | last | select(test("^[0-9]+\\.[0-9]+\\.[0-9]+$"))' "$tmp/page" >> "$tmp/versions"
  token=$(jq -r '.nextPageToken // ""' "$tmp/page")
  [ -z "$token" ] && break
done

# The newest patch of each minor from 2.19.
newest=$(jq -R -s -r '
  split("\n") | map(select(length > 0) | split(".") | map(tonumber))
  | map(select(.[0] > 2 or (.[0] == 2 and .[1] >= 19)))
  | group_by(.[0:2]) | map(max) | sort | map(map(tostring) | join(".")) | .[]' "$tmp/versions")

: > "$tmp/dart"
for v in $newest; do
  for p in $platforms; do
    # A platform the release does not publish answers 404; any other
    # failure stops the script, so a table never drops a pin it should hold.
    code=$(curl -sSL -o "$tmp/sum" -w '%{http_code}' "$archive/$v/sdk/dartsdk-$p-release.zip.sha256sum")
    case "$code" in
      200) ;;
      404) continue ;;
      *) echo "$v $p: HTTP $code" >&2; exit 1 ;;
    esac
    digest=$(awk '{print $1}' "$tmp/sum")
    echo "$digest" | grep -Eq '^[0-9a-f]{64}$' || { echo "$v $p: $digest is no SHA-256" >&2; exit 1; }
    printf '%s %s %s\n' "$v" "$p" "$digest" >> "$tmp/dart"
  done
done

for os in linux macos windows; do
  curl -fsSL "$flutter/releases_$os.json" > "$tmp/flutter-$os"
done

jq -n --rawfile dart "$tmp/dart" \
  --slurpfile linux "$tmp/flutter-linux" --slurpfile macos "$tmp/flutter-macos" --slurpfile windows "$tmp/flutter-windows" '
def flutter($m):
  {
    base_url: $m.base_url,
    releases: [$m.releases[]
      | select(.channel == "stable" and (.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+$")))
      | . + {arch: (.dart_sdk_arch // "x64"), n: (.version | split(".") | map(tonumber))}
      | select(.n[0] > 3 or (.n[0] == 3 and .n[1] >= 10))]
      | group_by([.arch, .n[0], .n[1]]) | map(max_by(.n)) | sort_by(.arch, .n)
      | map({channel, version, dart_sdk_version, dart_sdk_arch: .arch, archive, sha256})
  };
{
  dart: ($dart | split("\n") | map(select(length > 0) | split(" ") | {version: .[0], platform: .[1], digest: .[2]})
    | group_by(.version) | map({version: .[0].version, digests: (map({key: .platform, value: .digest}) | from_entries)})
    | sort_by(.version | split(".") | map(tonumber))),
  flutter: {linux: flutter($linux[0]), macos: flutter($macos[0]), windows: flutter($windows[0])}
}' > "$out"
echo "wrote $out"
