package cachekey

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func TestExtensionRuntimeDigestDeterministicAndComplete(t *testing.T) {
	root := t.TempDir()
	mustWriteRuntimeFile(t, filepath.Join(root, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(root, "cmd", "main.go"), "package main\n", 0o644)
	ext := runtimeTestExtension(root)

	first, err := ExtensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ExtensionRuntimeDigest(ext)
	if err != nil || first != second {
		t.Fatalf("determinism: %q, %q, %v", first, second, err)
	}

	mustWriteRuntimeFile(t, filepath.Join(root, "cmd", "main.go"), "package main\n// changed\n", 0o644)
	changedInput, _ := ExtensionRuntimeDigest(ext)
	if changedInput == first {
		t.Fatal("declared input content did not invalidate runtime digest")
	}
	ext.Name = "@putnami/other"
	changedIdentity, _ := ExtensionRuntimeDigest(ext)
	if changedIdentity == changedInput {
		t.Fatal("extension identity did not invalidate runtime digest")
	}
	ext.Runtime.Prepare.Args = append(ext.Runtime.Prepare.Args, "--changed")
	changedDeclaration, _ := ExtensionRuntimeDigest(ext)
	if changedDeclaration == changedIdentity {
		t.Fatal("prepare declaration did not invalidate runtime digest")
	}
}

func TestCollectRuntimeInputsRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	root := t.TempDir()
	mustWriteRuntimeFile(t, filepath.Join(root, "real"), "x", 0o644)
	if err := os.Symlink("real", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectRuntimeInputs(root, []string{"**"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("CollectRuntimeInputs error = %v, want symlink rejection", err)
	}
}

func writeRuntimeReplacementFixture(t *testing.T, root string) (string, string) {
	t.Helper()
	extensionRoot := filepath.Join(root, "extension")
	sdkRoot := filepath.Join(root, "sdk")
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "go.mod"), `module example.dev/extension

go 1.25

require example.dev/sdk v0.0.0

replace example.dev/sdk => ../sdk
`, 0o644)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "cmd", "main.go"), "package main\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "go.mod"), "module example.dev/sdk\n\ngo 1.25\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "sdk.go"), "package sdk\n", 0o644)
	return extensionRoot, sdkRoot
}

// TestExtensionRuntimeDigestFollowsThePrepareToolchainIdentity pins the input
// that makes toolchain pinning observable: the prepared binary is stored under
// a digest that includes WHICH compiler produced it. Without it, upgrading the
// locked toolchain would keep serving the artifact the previous one built.
func TestExtensionRuntimeDigestFollowsThePrepareToolchainIdentity(t *testing.T) {
	root := t.TempDir()
	writeRuntimeFixture(t, root, "@putnami/test", "1.2.3")
	ext := runtimeTestExtension(root)
	ext.Runtime.Toolchains = map[string]extensionproto.RuntimeToolchain{
		"compiler": {Lock: "compiler"},
	}
	ext.Runtime.Prepare.Toolchains = []string{"compiler"}
	ext.RuntimeToolchains = map[string]model.RuntimeToolchainResolution{
		"compiler": {Available: true, Identity: "identity-a"},
	}

	first, err := ExtensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	ext.RuntimeToolchains["compiler"] = model.RuntimeToolchainResolution{Available: true, Identity: "identity-b"}
	second, err := ExtensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("runtime digest did not move with the resolved compiler identity: %q", first)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "identity-keys-results",
		"a-prepared-runtime-digest-follows-the-resolved-identity")
}

func runtimeTestExtension(root string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name:        "@putnami/test",
		Version:     "1.2.3",
		Path:        root,
		LocalSource: true,
		Runtime: &extension.RuntimeDefinition{
			Executable: "bin/runtime",
			Prepare: &extension.RuntimePrepare{
				Command: "{extensionRoot}/bin/prepare",
				Args:    []string{"--output", "{runtimeOutput}"},
				Inputs:  []string{"bin/**", "cmd/**"},
			},
		},
	}
}

type runtimeFixtureControls struct {
	// prepareAssertions is shell run at the very start of the prepare, inside
	// the staged source view, so a test can assert on the tree the preparation
	// actually receives and fail the prepare when it is wrong.
	prepareAssertions string
	counterPath       string
	barrierPath       string
	barrierCount      int
	prepareDelay      time.Duration
	cpuWorkIterations int
	releasePath       string
	mutationTrigger   string
	mutationTarget    string
}

func writeRuntimeFixture(t *testing.T, root, name, version string) {
	t.Helper()
	writeRuntimeFixtureWithControls(t, root, name, version, runtimeFixtureControls{})
}

// writeRuntimeFixtureWithControls embeds test-owned coordination paths in the
// generated prepare script. Unlike process environment, these immutable
// per-fixture dependencies cannot bleed between parallel top-level tests. The
// environment-backed defaults remain for TestRuntimeSyncHelperProcess, where
// separate child processes deliberately use environment as their transport.
func writeRuntimeFixtureWithControls(
	t *testing.T,
	root, name, version string,
	controls runtimeFixtureControls,
) {
	t.Helper()
	info := fmt.Sprintf(
		`{"extension":%q,"version":%q,"platform":%q,"cliContract":4,"runtimeProtocol":%d,"runtimeABI":%d}`,
		name, version, runtime.GOOS+"/"+runtime.GOARCH,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	runtimeScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n" +
		"  printf '%s\\n' '" + info + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"parity\" ]; then\n" +
		"  printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"extension\":\"@putnami/test\",\"job\":\"parity\"}}'\n" +
		"  printf '%s\\n' '{\"v\":2,\"type\":\"log\",\"level\":\"info\",\"message\":\"structured\"}'\n" +
		"  printf '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"success\",\"data\":{\"cwd\":\"%s\",\"structured\":true}}}\\n' \"$PWD\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"cancel\" ]; then\n" +
		"  printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"extension\":\"@putnami/test\",\"job\":\"cancel\"}}'\n" +
		"  trap 'exit 0' TERM INT\n" +
		"  while :; do sleep 1; done\n" +
		"fi\n" +
		"exit 0\n"
	// The barrier is TestIndependentRuntimeSyncsRunConcurrently's rendezvous: the
	// prepare announces itself and refuses to finish until the whole cohort has
	// announced too. Under a serial stage the first prepare waits for peers that
	// cannot start, times out and EXITS NON-ZERO, so a regression is a failure
	// rather than a slow pass. The environment form is retained for the child-
	// process fixture; ordinary tests embed their test-owned path instead.
	counterStep := `if [ -n "${RUNTIME_PREPARE_COUNTER:-}" ]; then
  printf 'prepare\n' >> "$RUNTIME_PREPARE_COUNTER"
fi
`
	if controls.counterPath != "" {
		counterStep = "printf 'prepare\\n' >> " + shellQuote(controls.counterPath) + "\n"
	}
	barrierStep := `if [ -n "${RUNTIME_BARRIER:-}" ]; then
  printf 'here\n' >> "$RUNTIME_BARRIER"
  waited=0
  while [ "$(wc -l < "$RUNTIME_BARRIER" | tr -d ' ')" -lt "$RUNTIME_BARRIER_N" ]; do
    waited=$((waited + 1))
    if [ "$waited" -gt 300 ]; then
      printf 'prepare: cohort of %s never assembled; the stage ran serially\n' "$RUNTIME_BARRIER_N" >&2
      exit 3
    fi
    sleep 0.05
  done
fi
`
	if controls.barrierPath != "" {
		path := shellQuote(controls.barrierPath)
		barrierStep = fmt.Sprintf(`printf 'here\n' >> %s
waited=0
while [ "$(wc -l < %s | tr -d ' ')" -lt %d ]; do
  waited=$((waited + 1))
  if [ "$waited" -gt 300 ]; then
    printf 'prepare: cohort of %%s never assembled; the stage ran serially\n' %s >&2
    exit 3
  fi
  sleep 0.05
done
`, path, path, controls.barrierCount, shellQuote(fmt.Sprint(controls.barrierCount)))
	}
	delayStep := `[ -z "${RUNTIME_PREPARE_SLEEP:-}" ] || sleep "$RUNTIME_PREPARE_SLEEP"
`
	if controls.prepareDelay > 0 {
		delayStep = fmt.Sprintf("sleep %.6f\n", controls.prepareDelay.Seconds())
	}
	cpuWorkStep := ""
	if controls.cpuWorkIterations > 0 {
		cpuWorkStep = fmt.Sprintf(`i=0
while [ "$i" -lt %d ]; do
  i=$((i + 1))
done
`, controls.cpuWorkIterations)
	}
	releaseStep := ""
	if controls.releasePath != "" {
		release := shellQuote(controls.releasePath)
		releaseStep = fmt.Sprintf(`waited=0
while [ ! -f %s ]; do
  waited=$((waited + 1))
  if [ "$waited" -gt 6000 ]; then
    printf 'prepare: release was never published\n' >&2
    exit 4
  fi
  sleep 0.01
done
`, release)
	}
	mutationStep := `[ -z "${RUNTIME_MUTATE_FILE:-}" ] || printf 'mutation\n' >> "$RUNTIME_MUTATE_FILE"
`
	if controls.mutationTrigger != "" {
		mutationStep = fmt.Sprintf(`if [ -f %s ]; then
  rm -f %s
  printf 'mutation\n' >> %s
fi
`, shellQuote(controls.mutationTrigger), shellQuote(controls.mutationTrigger),
			shellQuote(controls.mutationTarget))
	}
	// The prepare runs the runtime it wrote once, as fixtureproc.Write does
	// for each program it places, so the handshake that follows within its
	// deadline does not also pay for the host's first-launch check of the new
	// file.
	prepare := `#!/bin/sh
set -eu
` + controls.prepareAssertions + counterStep + barrierStep + delayStep + releaseStep + cpuWorkStep + `output="$2"
mkdir -p "$output/bin"
cp bin/runtime-template "$output/bin/runtime"
chmod +x "$output/bin/runtime"
"$output/bin/runtime" __putnami runtime-info >/dev/null
` + mutationStep
	mustWriteRuntimeFile(t, filepath.Join(root, "bin", "prepare"), prepare, 0o755)
	mustWriteRuntimeFile(t, filepath.Join(root, "bin", "runtime-template"), runtimeScript, 0o755)
	mustWriteRuntimeFile(t, filepath.Join(root, "cmd", "source"), "source", 0o644)
}

func mustWriteRuntimeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if mode&0o111 != 0 {
		writeExecutable(t, path, content)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
