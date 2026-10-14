package jobs

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// contributorPreparationEnvironment is the environment the host prepares the
// contributor runtime from: a PATH that carries no Go compiler, no GOROOT,
// and only the home, temporary and Go cache and proxy locations of this
// process. The putnami-home candidate is then the only one that can resolve.
func contributorPreparationEnvironment() []string {
	env := []string{"PATH=/usr/bin:/bin"}
	for _, name := range []string{
		"HOME", "PUTNAMI_HOME", "TMPDIR",
		"GOCACHE", "GOMODCACHE", "GOPATH",
		"GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOPROXY", "GOPRIVATE", "GOINSECURE",
	} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// TestTheHostPreparesTheContributorRuntimeWithTheLockedGo runs the real
// bin/prepare of @putnami/contributor through the host's runtime preparation.
// The host resolves the prepare toolchain from the workspace lock, runs
// bin/prepare in an isolated source view and store, and accepts the result
// only after its handshake. The runtime it returns answers the handshake for
// this host and was built by the Go release putnami.lock.json pins, without
// cgo and with trimmed paths. The test skips when that release is not
// installed under the Putnami home.
func TestTheHostPreparesTheContributorRuntimeWithTheLockedGo(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bin/prepare is a bash script")
	}
	repoRoot := findJobsRepoRoot(t)
	locked, err := lockfile.ReadLockFile(repoRoot)
	if err != nil || locked == nil {
		t.Fatalf("read putnami.lock.json: %v", err)
	}
	pin, ok := locked.GetToolchain("go")
	if !ok || pin.Version == "" {
		t.Fatal("putnami.lock.json pins no Go release")
	}

	extensionRoot := filepath.Join(repoRoot, "tooling", "contributor")
	manifest, err := extension.LoadManifest(filepath.Join(extensionRoot, extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	ext := extension.Resolve(manifest, extensionRoot)
	ext.Name = manifest.Name
	ext.LocalSource = true
	if ext.Runtime == nil || ext.Runtime.Prepare == nil {
		t.Fatal("the contributor manifest declares no prepared runtime")
	}

	base := contributorPreparationEnvironment()
	for _, ref := range ext.Runtime.Prepare.Toolchains {
		for _, candidate := range ext.Runtime.Toolchains[ref].Candidates {
			if candidate.From != extensionproto.RuntimeToolchainCandidatePutnamiHome {
				continue
			}
			path := runtimeToolchainCandidatePath(repoRoot, candidate, pin.Version, base)
			if _, err := os.Stat(path); err != nil {
				t.Skipf("Go %s is not installed under the Putnami home (%s): %v", pin.Version, path, err)
			}
		}
	}

	ctx := t.Context()
	exts := []*extension.ExtensionDescription{ext}
	if err := resolveRuntimeToolchainsForPreparation(ctx, repoRoot, exts, base, false); err != nil {
		t.Fatalf("resolve the prepare toolchains: %v", err)
	}
	for _, ref := range ext.Runtime.Prepare.Toolchains {
		if resolution := ext.RuntimeToolchains[ref]; !resolution.Available {
			t.Fatalf("prepare toolchain %q is unavailable: %+v", ref, resolution)
		}
	}
	if err := synchronizeExtensionRuntimesWithEnv(ctx, exts, nil, artifactstore.New(t.TempDir()), base); err != nil {
		t.Fatalf("prepare the contributor runtime: %v", err)
	}

	var stdout, stderr bytes.Buffer
	handshake := exec.CommandContext(ctx, ext.RuntimeExecutable, "__putnami", "runtime-info")
	handshake.Env = base
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
	if want := "go" + pin.Version; built.GoVersion != want {
		t.Errorf("the runtime was built by %s, want %s, the release putnami.lock.json pins", built.GoVersion, want)
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
