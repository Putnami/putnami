#!/bin/sh
# Writes sdkreleases.json, the .NET SDK and runtime releases @putnami/dotnet
# installs: for every channel from 8.0 that Microsoft's release index lists,
# the newest SDK of each feature band and the newest ASP.NET Core runtime,
# with the archive and the SHA-512 Microsoft publishes for each platform the
# extension installs on. A
# pin moves only when a release of the extension ships a regenerated file
# (ADR 0002 of @putnami/dotnet).
#
#   dotnet/extension/scripts/sdk-releases.sh
#
# It needs curl and jq.
set -eu

out="$(cd "$(dirname "$0")/.." && pwd)/sdkreleases.json"
base="https://builds.dotnet.microsoft.com/dotnet/release-metadata"
rids='["osx-x64","osx-arm64","linux-x64","linux-arm64","linux-musl-x64","linux-musl-arm64","win-x64","win-arm64"]'

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$base/releases-index.json" > "$tmp/index"
channels=$(jq -r '."releases-index"[] | ."channel-version" | select(split(".") | map(tonumber) | .[0] >= 8)' "$tmp/index")
for ch in $channels; do
  url=$(jq -r --arg ch "$ch" '."releases-index"[] | select(."channel-version" == $ch) | ."releases.json"' "$tmp/index")
  curl -fsSL "$url" > "$tmp/channel-$ch"
done

jq -n --argjson rids "$rids" --slurpfile index "$tmp/index" '
def files($prefix):
  [(.files // [])[]
    | select(.rid as $r | $rids | index($r))
    | select(.name == ($prefix + "-" + .rid + (if (.rid | startswith("win-")) then ".zip" else ".tar.gz" end)))
    | {name, rid, url, hash}
    | if (.hash | test("^[0-9a-f]{128}$")) then . else error("\(.name): \(.hash) is no SHA-512") end];
def published($prefix): if . == null then null else {version, files: files($prefix)} end;
{
  "releases-index": [$index[0]."releases-index"[]
    | select(."channel-version" | split(".") | map(tonumber) | .[0] >= 8)
    | {"channel-version", "support-phase", "releases.json": ."channel-version"}],
  channels: {}
}' > "$tmp/out"

for ch in $channels; do
  jq --arg ch "$ch" --argjson rids "$rids" --slurpfile c "$tmp/channel-$ch" '
def files($prefix):
  [(.files // [])[]
    | select(.rid as $r | $rids | index($r))
    | select(.name == ($prefix + "-" + .rid + (if (.rid | startswith("win-")) then ".zip" else ".tar.gz" end)))
    | {name, rid, url, hash}];
def published($prefix): {version, files: files($prefix)};
def release($v): $v | split("-")[0] | split(".") | map(tonumber);
($c[0].releases | map(.sdk, (.sdks // [])[]) | map(select(. != null and (.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+$")))) | unique_by(.version)) as $sdks
| ($c[0].releases | map(."aspnetcore-runtime" | select(. != null and (.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+$")))) | max_by(release(.version))) as $runtime
| .channels[$ch] = {releases: (
    [{"aspnetcore-runtime": ($runtime | published("aspnetcore-runtime")), sdks: []}]
    + [{sdks: [$sdks | group_by(release(.version) | .[2] / 100 | floor) | map(max_by(release(.version)))[] | published("dotnet-sdk")] | sort_by(release(.version)) | reverse}]
  )}
' "$tmp/out" > "$tmp/next" && mv "$tmp/next" "$tmp/out"
done

jq '.' "$tmp/out" > "$out"
echo "wrote $out"
