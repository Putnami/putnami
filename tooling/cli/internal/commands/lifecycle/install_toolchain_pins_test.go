package lifecycle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchainpins"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// toolchainPinsFixture is the outcome of a toolchain pins phase that answered
// tool 1.2.3 and kept a pin of kept 4.5.6 that no extension answered, with the
// server that serves both archives and the count of its requests.
type toolchainPinsFixture struct {
	root     string
	pins     toolchainpins.Result
	requests *atomic.Int32
}

func newToolchainPinsFixture(t *testing.T) toolchainPinsFixture {
	t.Helper()
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	archive := toolchainPinArchive(t)
	sum := sha256.Sum256(archive)
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	platform := lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)
	pin := func(identity, version string) lockfile.LockEntry {
		return lockfile.LockEntry{
			Version:     version,
			Integrities: map[string]string{platform: hex.EncodeToString(sum[:])},
			Archives: map[string]lockfile.LockArchive{platform: {
				URL: server.URL + "/" + identity + "-" + version + ".tar.gz", Format: "tar.gz", Executable: "bin/tool",
			}},
		}
	}
	return toolchainPinsFixture{
		root: t.TempDir(),
		pins: toolchainpins.Result{
			Toolchains: map[string]lockfile.LockEntry{"tool": pin("tool", "1.2.3"), "kept": pin("kept", "4.5.6")},
			Answers:    map[string]toolchainpins.Answer{"tool": {Extension: "@acme/tool"}},
		},
		requests: requests,
	}
}

// toolchainPinArchive is a tar.gz that holds the executable bin/tool.
func toolchainPinArchive(t *testing.T) []byte {
	t.Helper()
	content := "#!/bin/sh\necho 1.2.3\n"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "bin/tool", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The pins phase installs the pinned release of each identity an extension
// answered for under the Putnami home, and says so. A pin no extension
// answered for is not installed, and a second run finds the release in place
// and requests nothing.
func TestInstallToolchainPins_InstallsEachAnsweredReleaseTheMachineLacks(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-pinned-archive-is-verified-and-unpacked")
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-complete-folder-is-reused-without-a-request")
	fixture := newToolchainPinsFixture(t)

	// With no OnAction, the line goes to Out.
	var out bytes.Buffer
	if err := installToolchainPins(context.Background(), fixture.root, fixture.pins, toolchainpins.Options{}, LifecycleEnv{Out: &out}); err != nil {
		t.Fatalf("install the toolchain pins: %v", err)
	}
	if got, want := out.String(), "  ✓ Installed tool 1.2.3 under the Putnami home\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	tool := filepath.Join(toolchain.ProvisionedFolder(fixture.root, "tool", "1.2.3"), "bin", "tool")
	if data, err := os.ReadFile(tool); err != nil || !strings.Contains(string(data), "echo 1.2.3") {
		t.Fatalf("installed tool = %q, %v; want the archive's bin/tool", data, err)
	}
	if _, err := os.Stat(toolchain.ProvisionedFolder(fixture.root, "kept", "4.5.6")); !os.IsNotExist(err) {
		t.Fatalf("the pin no extension answered for was installed: %v", err)
	}
	if got := fixture.requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want one: the answered archive only", got)
	}

	// With OnAction, nothing is installed again, so nothing is reported.
	var actions []LifecycleAction
	env := LifecycleEnv{Out: &out, OnAction: func(action LifecycleAction) { actions = append(actions, action) }}
	if err := installToolchainPins(context.Background(), fixture.root, fixture.pins, toolchainpins.Options{}, env); err != nil {
		t.Fatalf("install the toolchain pins again: %v", err)
	}
	if len(actions) != 0 || fixture.requests.Load() != 1 {
		t.Fatalf("second run reported %+v after %d requests; want no action and no request", actions, fixture.requests.Load())
	}
}

// The install reports each release it installs to OnAction, as `putnami
// install` composes its summary.
func TestInstallToolchainPins_ReportsAnInstalledReleaseToOnAction(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-pinned-archive-is-verified-and-unpacked")
	fixture := newToolchainPinsFixture(t)
	var out bytes.Buffer
	var actions []LifecycleAction
	env := LifecycleEnv{Out: &out, OnAction: func(action LifecycleAction) { actions = append(actions, action) }}
	if err := installToolchainPins(context.Background(), fixture.root, fixture.pins, toolchainpins.Options{}, env); err != nil {
		t.Fatalf("install the toolchain pins: %v", err)
	}
	want := []LifecycleAction{{Kind: "toolchain", Name: "tool", Description: "Installed tool 1.2.3 under the Putnami home"}}
	if !slices.Equal(actions, want) || out.Len() != 0 {
		t.Fatalf("actions = %+v, output %q; want %+v and no output", actions, out.String(), want)
	}
}

// A hosted run, or one whose dependencies are offline, installs nothing and
// requests nothing: it runs the toolchains the machine holds.
func TestInstallToolchainPins_AHostedOrOfflineRunInstallsNothing(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-hosted-or-offline-run-installs-nothing")
	for name, opts := range map[string]toolchainpins.Options{
		"hosted":  {Hosted: true, Offline: true},
		"offline": {Offline: true},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newToolchainPinsFixture(t)
			var out bytes.Buffer
			if err := installToolchainPins(context.Background(), fixture.root, fixture.pins, opts, LifecycleEnv{Out: &out}); err != nil {
				t.Fatalf("install the toolchain pins: %v", err)
			}
			if _, err := os.Stat(toolchain.ProvisionedFolder(fixture.root, "tool", "1.2.3")); !os.IsNotExist(err) {
				t.Fatalf("the %s run installed tool 1.2.3: %v", name, err)
			}
			if got := fixture.requests.Load(); got != 0 || out.Len() != 0 {
				t.Fatalf("the %s run made %d requests and printed %q; want none", name, got, out.String())
			}
		})
	}
}
