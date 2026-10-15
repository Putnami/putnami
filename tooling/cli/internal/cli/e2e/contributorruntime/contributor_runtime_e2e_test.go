// Package contributorruntime prepares the runtime of the real
// @putnami/contributor extension through the host's exported runtime stage,
// with the workspace lock of this repository. The stage reads the artifact
// store location from the environment, so these tests set it and run in their
// own test binary.
package contributorruntime

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// repositoryRoot is the workspace root above the test's working directory.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no putnami.workspace.json above the test's working directory")
		}
		dir = parent
	}
}

// TestTheHostPreparesTheContributorRuntimeWithTheLockedGo runs the real
// bin/prepare of @putnami/contributor through jobs.SynchronizeExtensionRuntimes.
// The host resolves the prepare toolchain from putnami.lock.json against this
// process's environment, where the Go extension's test task sets GOROOT to the
// pinned Go release. It runs bin/prepare in an isolated source view, stores
// the result in a fresh artifact store, and accepts it only after the
// runtime handshake. The runtime it returns answers the handshake for this
// host and was built by the pinned release, without cgo and with trimmed
// paths. The test runs only in a test binary that the pinned release built.
func TestTheHostPreparesTheContributorRuntimeWithTheLockedGo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bin/prepare is a bash script")
	}
	repoRoot := repositoryRoot(t)
	locked, err := lockfile.ReadLockFile(repoRoot)
	if err != nil || locked == nil {
		t.Fatalf("read putnami.lock.json: %v", err)
	}
	pin, ok := locked.GetToolchain("go")
	if !ok || pin.Version == "" {
		t.Fatal("putnami.lock.json pins no Go release")
	}
	wantGo := "go" + pin.Version
	if runtime.Version() != wantGo {
		t.Skipf("this test binary was built by %s, not by %s, the release putnami.lock.json pins", runtime.Version(), wantGo)
	}

	extensionRoot := filepath.Join(repoRoot, "tooling", "contributor")
	manifest, err := extension.LoadManifest(filepath.Join(extensionRoot, extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	ext := extension.Resolve(manifest, extensionRoot)
	ext.Name = manifest.Name
	ext.LocalSource = true
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	ctx := context.Background()
	if err := jobs.SynchronizeExtensionRuntimes(
		ctx, &workspace.Workspace{Root: repoRoot}, []*extension.ExtensionDescription{ext}, nil,
	); err != nil {
		t.Fatalf("prepare the contributor runtime with GOROOT=%q: %v", os.Getenv("GOROOT"), err)
	}

	var stdout, stderr bytes.Buffer
	handshake := exec.CommandContext(ctx, ext.RuntimeExecutable, "__putnami", "runtime-info")
	handshake.Stdout = &stdout
	handshake.Stderr = &stderr
	if err := handshake.Run(); err != nil {
		t.Fatalf("runtime-info: %v: %s", err, stderr.String())
	}
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	var info runtimeproto.Info
	if err := decoder.Decode(&info); err != nil {
		t.Fatalf("runtime-info is not a descriptor: %v", err)
	}
	want := runtimeproto.Info{
		Extension:       manifest.Name,
		Version:         ext.Version,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	}
	if info != want {
		t.Errorf("runtime-info %+v, want %+v", info, want)
	}

	built, err := buildinfo.ReadFile(ext.RuntimeExecutable)
	if err != nil {
		t.Fatalf("read the runtime's build information: %v", err)
	}
	if built.GoVersion != wantGo {
		t.Errorf("the runtime was built by %s, want %s, the release putnami.lock.json pins", built.GoVersion, wantGo)
	}
	settings := map[string]string{}
	for _, setting := range built.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["CGO_ENABLED"] != "0" || settings["-trimpath"] != "true" {
		t.Errorf("the runtime was built with CGO_ENABLED=%q and -trimpath=%q, want 0 and true",
			settings["CGO_ENABLED"], settings["-trimpath"])
	}
}
