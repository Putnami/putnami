#!/usr/bin/env bash
# Raw-toolchain baseline: the same Go work as the Putnami gate, without Putnami.
set +e
mode="$1"
mods() { go list -m -f '{{.Dir}}'; }
cfg() {
  d="$1"
  while [ "$d" != "$GITHUB_WORKSPACE" ] && [ "$d" != "/" ]; do
    for n in .golangci.yml .golangci.yaml; do [ -f "$d/$n" ] && { echo "$d/$n"; return; }; done
    d="$(dirname "$d")"
  done
  echo "$GITHUB_WORKSPACE/go/extension/config/.golangci.yml"
}
export -f cfg
case "$mode" in
  test-one)      go test -timeout 30m -cover $(go list -m -f '{{.Path}}/...') ;;
  test-permod)   mods | xargs -P4 -I{} bash -c 'cd {} && go test -timeout 30m -cover ./... 2>&1 | tail -n 3' ;;
  test-permod-coverpkg) mods | xargs -P4 -I{} bash -c 'cd {} && go test -timeout 30m -cover -coverpkg=./... ./... 2>&1 | tail -n 3' ;;
  cli-cover)     cd tooling/cli && go test -timeout 30m -cover ./... ;;
  cli-coverpkg)  cd tooling/cli && go test -timeout 30m -cover -coverpkg=./... ./... ;;
  lint-permod)   mods | xargs -P4 -I{} bash -c 'cd {} && golangci-lint run --config "$(cfg {})" ./... >/dev/null 2>&1; staticcheck ./... >/dev/null 2>&1; echo "linted {}"' ;;
  build-one)     go build $(go list -m -f '{{.Path}}/...') ;;
esac
echo "mode $mode done"
