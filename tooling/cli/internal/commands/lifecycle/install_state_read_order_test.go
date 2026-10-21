package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The first-use bootstrap reads the install inputs from the workspace index
// before the command's own workspace sync, and the sync is what records a
// changed adapter declaration. These tests drive that order with the real
// index, the real sync and the real bootstrap. Only the install is a double:
// like Install, it synchronizes the workspace (its workspace-install run does)
// and then writes the marker.

// writePreviousMarker writes the marker the way the previous install-state
// code wrote it for the given root files: format version 1, each file with its
// size, mtime and sha256, and a fingerprint over "path NUL hash NUL" in name
// order. The encoding is spelled out here, not taken from writeInstallState,
// so a change to the marker format fails these tests.
func writePreviousMarker(t *testing.T, ws string, names ...string) {
	t.Helper()
	names = slices.Sorted(slices.Values(names))
	fingerprint := sha256.New()
	files := make([]map[string]any, 0, len(names))
	for _, name := range names {
		path := filepath.Join(ws, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		files = append(files, map[string]any{
			"path": name, "size": info.Size(), "mtimeNs": info.ModTime().UnixNano(), "hash": hash,
		})
		fingerprint.Write([]byte(name + "\x00" + hash + "\x00"))
	}
	encoded, err := json.MarshalIndent(map[string]any{
		"version":     1,
		"installedAt": "2026-10-01T00:00:00Z",
		"fingerprint": hex.EncodeToString(fingerprint.Sum(nil)),
		"lockFiles":   files,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".putnami", "install-state.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A marker the previous code wrote reads as it did: current while the files
// it recorded are the files the index declares, with the same content.
func TestEvalInstall_AMarkerThePreviousCodeWroteStaysReadable(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	p := writeLock(t, ws, "bun.lock", "v1")
	declareInstallInputs(t, ws, "bun.lock")
	writePreviousMarker(t, ws, "bun.lock", "putnami.lock.json")

	if stale, repair := evalInstall(ws); stale || repair != nil {
		t.Fatalf("a previous marker over unchanged files: stale=%v repair=%v, want current", stale, repair != nil)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	moved := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(p, moved, moved); err != nil {
		t.Fatal(err)
	}
	if stale, repair := evalInstall(ws); stale || repair == nil {
		t.Fatalf("a previous marker after an mtime move: stale=%v repair=%v, want current with a repair", stale, repair != nil)
	}
	rewriteLock(t, ws, "bun.lock", "v2")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a previous marker stayed current after a declared install input changed")
	}
}

// readOrderWorkspace is a workspace with one project and one provider whose
// adapter declaration a test edits between commands.
type readOrderWorkspace struct {
	t        *testing.T
	root     string
	ws       *workspace.Workspace
	provider *fixtureProbeProvider
	// installInputs is what the provider's adapter declares now.
	installInputs []string
	// installs counts the installs the bootstrap ran.
	installs int
}

// fixtureProbeProvider answers every probe with no project.
type fixtureProbeProvider struct{ calls int }

func (p *fixtureProbeProvider) Name() string { return "@fixture/adapter" }

func (p *fixtureProbeProvider) Probe(wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	p.calls++
	return wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: p.Name()}, nil
}

func newReadOrderWorkspace(t *testing.T) *readOrderWorkspace {
	t.Helper()
	resetBootstrapEnv(t)
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["app"]}`)
	writeProjectFile(t, filepath.Join(root, "app", "fixture.toml"), "name = \"app\"\n")
	writeLock(t, root, "putnami.lock.json", `{}`)
	writeLock(t, root, "bun.lock", "v1")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	f := &readOrderWorkspace{t: t, root: root, ws: ws, provider: &fixtureProbeProvider{}}

	previous := installRunner
	t.Cleanup(func() { installRunner = previous })
	installRunner = func(context.Context, string, *wsproto.Config, []string, LifecycleEnv) error {
		f.installs++
		f.sync()
		return writeInstallState(root)
	}
	t.Cleanup(func() { bootstrappedWorkspaces.Delete(root) })
	return f
}

// sync is one workspace sync over the current declaration, as a run's engine
// performs it.
func (f *readOrderWorkspace) sync() {
	f.t.Helper()
	scope, ok := workspace.NewProviderScope(f.provider.Name(), &extproto.WorkspaceAdapter{
		Markers:       []string{"fixture.toml"},
		Inputs:        []string{"fixture.toml", "bun.lock"},
		InstallInputs: f.installInputs,
	})
	if !ok {
		f.t.Fatal("NewProviderScope rejected the fixture adapter")
	}
	if _, err := workspace.Synchronize(workspace.SyncRequest{
		Workspace: f.ws,
		Providers: []workspace.ProviderBinding{{Scope: scope, Provider: f.provider, Implementation: "runtime-1"}},
		Reason:    wsproto.ProbeReasonLoad,
	}); err != nil {
		f.t.Fatalf("Synchronize: %v", err)
	}
}

// command runs one command in a new process, in the order the terminal path
// runs it: the first-use bootstrap, then the run's workspace sync. It reports
// whether the bootstrap installed.
func (f *readOrderWorkspace) command() bool {
	f.t.Helper()
	bootstrappedWorkspaces.Delete(f.root)
	if err := os.Setenv(workspaceBootstrappedEnv, ""); err != nil {
		f.t.Fatal(err)
	}
	before := f.installs
	if err := EnsureWorkspaceBootstrap(context.Background(), f.root, &wsproto.Config{}, BootstrapOptions{Command: "build"}); err != nil {
		f.t.Fatalf("EnsureWorkspaceBootstrap: %v", err)
	}
	f.sync()
	return f.installs > before
}

// recordedInstallInputs is what the index records for the provider now.
func (f *readOrderWorkspace) recordedInstallInputs() []string {
	f.t.Helper()
	snapshot, err := workspace.LoadSnapshot(f.root)
	if err != nil || snapshot == nil || len(snapshot.Providers) != 1 {
		f.t.Fatalf("LoadSnapshot = %v, %v; want one recorded provider", snapshot, err)
	}
	return snapshot.Providers[0].InstallInputs
}

// After an upgrade, the index records no install inputs and the marker counted
// the lockfile the previous code took from the probe inputs. The first command
// sees a smaller file set than the marker's, and installs once; that install
// synchronizes the index first, so its marker records the declaration and the
// next command is current.
func TestInstallState_AnIndexWithoutInstallInputsCostsOneInstallAfterAnUpgrade(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "install-state-inputs", "a-changed-declaration-counts-after-the-sync-that-records-it")
	f := newReadOrderWorkspace(t)
	f.sync()
	if got := f.recordedInstallInputs(); got != nil {
		t.Fatalf("the index before the upgrade records install inputs %v", got)
	}
	writePreviousMarker(t, f.root, "bun.lock", "putnami.lock.json")
	f.installInputs = []string{"bun.lock"}

	if !f.command() {
		t.Fatal("the first command after the upgrade did not install")
	}
	if got := f.recordedInstallInputs(); !slices.Equal(got, []string{"bun.lock"}) {
		t.Fatalf("recorded install inputs = %v, want [bun.lock]", got)
	}
	if f.command() {
		t.Fatal("the second command after the upgrade installed again")
	}
	rewriteLock(t, f.root, "bun.lock", "v2")
	if !f.command() {
		t.Fatal("a bun.lock edit after the upgrade did not install")
	}
	if f.command() {
		t.Fatal("the command after that install installed again")
	}
	if f.installs != 2 {
		t.Errorf("installs = %d, want 2: one for the upgrade, one for the edit", f.installs)
	}
}

// A local extension's edited declaration reaches the index in the first
// command's sync, after that command's bootstrap. The command after it counts
// the new file set and installs once. Removing the declaration takes the same
// path, and the removed file stops counting.
func TestInstallState_AnEditedDeclarationCountsFromTheCommandAfterItsSync(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "install-state-inputs", "a-changed-declaration-counts-after-the-sync-that-records-it")
	f := newReadOrderWorkspace(t)
	if !f.command() {
		t.Fatal("a workspace without a marker did not install")
	}
	if f.command() {
		t.Fatal("an installed workspace installed again")
	}

	f.installInputs = []string{"bun.lock"}
	if f.command() {
		t.Fatal("the bootstrap installed before a sync recorded the new declaration")
	}
	if got := f.recordedInstallInputs(); !slices.Equal(got, []string{"bun.lock"}) {
		t.Fatalf("recorded install inputs = %v, want [bun.lock]", got)
	}
	if !f.command() {
		t.Fatal("the command after the sync that recorded bun.lock did not install")
	}
	if f.command() {
		t.Fatal("the command after that install installed again")
	}
	rewriteLock(t, f.root, "bun.lock", "v2")
	if !f.command() {
		t.Fatal("a declared bun.lock changed and the next command did not install")
	}

	f.installInputs = nil
	if f.command() {
		t.Fatal("the bootstrap installed before a sync recorded the removed declaration")
	}
	if !f.command() {
		t.Fatal("the command after the sync that removed bun.lock did not install")
	}
	rewriteLock(t, f.root, "bun.lock", "v3")
	if f.command() {
		t.Fatal("bun.lock still counts after its declaration was removed")
	}
	if f.installs != 4 {
		t.Errorf("installs = %d, want 4", f.installs)
	}
}
