package lifecycle

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// recordProviders writes a workspace index that records the given providers,
// the file the install-state check reads the install inputs from.
func recordProviders(t *testing.T, ws string, providers ...workspace.SnapshotProvider) {
	t.Helper()
	snapshot := &workspace.Snapshot{Version: 4, ProbeDigest: "wp1:test", IdentityDigest: "wsid1:test"}
	for _, provider := range providers {
		if provider.Digest == "" {
			provider.Digest = "wp1:test"
		}
		provider.Result = wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension}
		snapshot.Providers = append(snapshot.Providers, provider)
	}
	if err := workspace.WriteSnapshot(ws, snapshot, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatal(err)
	}
}

// declareInstallInputs records one provider whose adapter declares the given
// install inputs.
func declareInstallInputs(t *testing.T, ws string, names ...string) {
	t.Helper()
	recordProviders(t, ws, workspace.SnapshotProvider{Extension: "@fixture/adapter", InstallInputs: names})
}

// probeInputs is the recorded probe-input list of a provider: names a probe
// reads, with no digest a test depends on.
func probeInputs(names ...string) []workspace.SnapshotInput {
	inputs := make([]workspace.SnapshotInput, 0, len(names))
	for _, name := range names {
		inputs = append(inputs, workspace.SnapshotInput{Path: name, Digest: "absent"})
	}
	return inputs
}

func writeLock(t *testing.T, ws, name, content string) string {
	t.Helper()
	p := filepath.Join(ws, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// rewriteLock replaces an existing lock file's content and moves its mtime one
// second past the stamp it had, so the rewrite is visible to evalInstall's
// stat-only fast path on every filesystem.
//
// A test must never leave that visibility to the filesystem's clock. The fast
// path compares size and mtime, so a rewrite that keeps the same LENGTH —
// "version = 1\n" to "version = 2\n" — is distinguished by the mtime alone. When
// the filesystem's timestamp granularity is coarser than the gap between the two
// writes, both stamps are identical, the fast path reports "unchanged", and the
// content oracle is never consulted. That is why the uv.lock case below passed on
// APFS, whose stamps are effectively per-write, and failed on CI.
//
// Deriving the new stamp from the old one rather than from time.Now() keeps the
// move strictly monotonic without depending on the clock's resolution.
func rewriteLock(t *testing.T, ws, name, content string) string {
	t.Helper()
	p := filepath.Join(ws, name)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatalf("rewriteLock needs an existing %s: %v", name, err)
	}
	writeLock(t, ws, name, content)
	moved := before.ModTime().Add(time.Second)
	if err := os.Chtimes(p, moved, moved); err != nil {
		t.Fatalf("move mtime of %s: %v", name, err)
	}
	return p
}

func TestCombinedFingerprint_DependsOnContentNotMtime(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	writeLock(t, wsA, "putnami.lock.json", `{"a":1}`)
	writeLock(t, wsB, "putnami.lock.json", `{"a":1}`)

	a, err := scanLockFiles(wsA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := scanLockFiles(wsB)
	if err != nil {
		t.Fatal(err)
	}
	if combinedFingerprint(a) != combinedFingerprint(b) {
		t.Error("identical content must produce identical fingerprints regardless of mtime")
	}

	writeLock(t, wsB, "putnami.lock.json", `{"a":2}`)
	b2, _ := scanLockFiles(wsB)
	if combinedFingerprint(a) == combinedFingerprint(b2) {
		t.Error("different content must produce different fingerprints")
	}
}

// A provider whose adapter declares a file in installInputs makes that file
// decide the install state: a content change stales the install.
func TestEvalInstall_ADeclaredInstallInputStalesTheInstall(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "install-state-inputs", "a-declared-install-input-stales-the-install")
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	writeLock(t, ws, "bun.lock", "v1")
	declareInstallInputs(t, ws, "bun.lock")
	if got := workspaceInstallStateInputs(ws); !slices.Equal(got, []string{"bun.lock", "putnami.lock.json"}) {
		t.Fatalf("inputs = %v, want the declared install input and the core's lock", got)
	}
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	if stale, repair := evalInstall(ws); stale || repair != nil {
		t.Fatalf("the install state is not current after the install: stale=%v repair=%v", stale, repair != nil)
	}
	rewriteLock(t, ws, "bun.lock", "v2")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a declared install input changed and the install state stayed current")
	}
}

// A provider that declares no install inputs adds no file to the install
// state, even a lockfile its probe reads. Editing that lockfile leaves the
// install current: the core names no language lockfile of its own.
func TestEvalInstall_AnUndeclaredLockfileLeavesTheInstallCurrent(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "install-state-inputs", "an-undeclared-lockfile-leaves-the-install-current")
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	writeLock(t, ws, "bun.lock", "v1")
	recordProviders(t, ws, workspace.SnapshotProvider{
		Extension: "@fixture/adapter",
		Inputs:    probeInputs("bun.lock", "package.json"),
	})
	if got := workspaceInstallStateInputs(ws); !slices.Equal(got, []string{"putnami.lock.json"}) {
		t.Fatalf("inputs = %v, want the core's lock alone", got)
	}
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	rewriteLock(t, ws, "bun.lock", "v2")
	if stale, _ := evalInstall(ws); stale {
		t.Error("a lockfile no provider declares as an install input staled the install")
	}
}

// The core's own lock decides the install state whatever the providers
// declare, and with no workspace index at all.
func TestEvalInstall_ThePutnamiLockAlwaysCounts(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "install-state-inputs", "the-putnami-lock-always-counts")
	for name, record := range map[string]func(t *testing.T, ws string){
		"no index":                    func(*testing.T, string) {},
		"a provider declaring none":   func(t *testing.T, ws string) { declareInstallInputs(t, ws) },
		"a provider declaring a file": func(t *testing.T, ws string) { declareInstallInputs(t, ws, "bun.lock") },
	} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			writeLock(t, ws, "putnami.lock.json", `{"v":1}`)
			record(t, ws)
			if err := writeInstallState(ws); err != nil {
				t.Fatal(err)
			}
			rewriteLock(t, ws, "putnami.lock.json", `{"v":2}`)
			if stale, _ := evalInstall(ws); !stale {
				t.Error("putnami.lock.json changed and the install state stayed current")
			}
		})
	}
}

// Probe inputs and install inputs answer different questions: a root file a
// probe reads decides what a project resolves to and installs nothing. Only
// the declared install inputs count.
func TestEvalInstall_ProbeInputsAreNotInstallInputs(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	writeLock(t, ws, "biome.json", `{"formatter":{"indentWidth":2}}`)
	writeLock(t, ws, "bun.lock", "v1")
	recordProviders(t, ws, workspace.SnapshotProvider{
		Extension:     "@fixture/adapter",
		Inputs:        probeInputs("biome.json", "bun.lock", "package.json", "tsconfig.base.json"),
		InstallInputs: []string{"bun.lock"},
	})

	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	writeLock(t, ws, "biome.json", `{"formatter":{"indentWidth":4}}`)
	if stale, repair := evalInstall(ws); stale || repair != nil {
		t.Errorf("editing a root probe input forced a re-install (stale=%v repair=%v)", stale, repair != nil)
	}
	rewriteLock(t, ws, "bun.lock", "v2")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a declared install input changed and the install state stayed current")
	}
}

// The install inputs of every recorded provider count, each name once.
func TestEvalInstall_EveryRecordedProviderContributesItsInstallInputs(t *testing.T) {
	ws := t.TempDir()
	recordProviders(t, ws,
		workspace.SnapshotProvider{Extension: "@fixture/a", InstallInputs: []string{"b.lock", "a.lock"}},
		workspace.SnapshotProvider{Extension: "@fixture/b", InstallInputs: []string{"a.lock", "c.lock"}},
		workspace.SnapshotProvider{Extension: "@fixture/c"},
	)
	want := []string{"a.lock", "b.lock", "c.lock", "putnami.lock.json"}
	if got := workspaceInstallStateInputs(ws); !slices.Equal(got, want) {
		t.Errorf("inputs = %v, want %v", got, want)
	}
}

// The index is a file in the workspace, and a hand-edited one can record a
// name no adapter could declare. The check skips every name the protocol
// refuses as an install input, so it never reads a file outside the root.
func TestEvalInstall_ARecordedNameTheProtocolRefusesIsSkipped(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	declareInstallInputs(t, ws, "bun.lock", "apps/web/bun.lock", "*.lock", "../outside.lock", "..", " bun.lockb", "")
	if got := workspaceInstallStateInputs(ws); !slices.Equal(got, []string{"bun.lock", "putnami.lock.json"}) {
		t.Errorf("inputs = %v, want the core's lock plus the one valid name", got)
	}
}

// An unreadable index adds nothing: the check falls back to the core's lock,
// and a marker that recorded more files no longer matches.
func TestEvalInstall_AnUnreadableIndexLeavesTheCoreLockAlone(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	writeLock(t, ws, "bun.lock", "v1")
	declareInstallInputs(t, ws, "bun.lock")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspace.SnapshotPath(ws), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := workspaceInstallStateInputs(ws); !slices.Equal(got, []string{"putnami.lock.json"}) {
		t.Fatalf("inputs = %v, want the core's lock alone", got)
	}
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a marker that recorded a file the check no longer reads stayed current")
	}
}

func TestEvalInstall_MissingMarkerIsStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a workspace with no marker must be stale (fresh checkout)")
	}
}

func TestEvalInstall_FreshAfterWrite(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{"x":1}`)
	writeLock(t, ws, "bun.lock", "lockfile content")
	declareInstallInputs(t, ws, "bun.lock")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	stale, repair := evalInstall(ws)
	if stale {
		t.Error("just-installed workspace must not be stale")
	}
	if repair != nil {
		t.Error("unchanged lock files need no fast-path repair")
	}
}

// A repository can commit a marker that matches its locks. A hosted run
// installs anyway: the install runs the workspace-fetch before any repository
// code, and a skipped install would leave no fetch that receives the job
// credential.
func TestEvalInstall_AHostedRunIgnoresACurrentMarker(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "fetch-completes-before-install")
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{"x":1}`)
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	if stale, _ := evalInstall(ws); stale {
		t.Fatal("without a run credential, the just-written marker must be current")
	}
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	if stale, repair := evalInstall(ws); !stale || repair != nil {
		t.Errorf("hosted evalInstall = %v, %v; want stale with no repair", stale, repair)
	}
}

func TestEvalInstall_ContentChangeIsStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "bun.lock", "v1")
	declareInstallInputs(t, ws, "bun.lock")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	writeLock(t, ws, "bun.lock", "v2-different-length")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("changed lock content must be stale")
	}
}

// A rewrite of a declared install input that keeps its length stales the
// install.
//
// The rewrite keeps the length a real `version = N` bump would keep, so it goes
// through rewriteLock: on a filesystem whose stamps are coarser than two
// back-to-back writes, a same-length rewrite is otherwise invisible and this test
// would assert nothing.
func TestEvalInstall_UVLockChangeIsStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "uv.lock", "version = 1\n")
	declareInstallInputs(t, ws, "uv.lock")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	rewriteLock(t, ws, "uv.lock", "version = 2\n")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("changed uv.lock content must be stale")
	}
}

// The stat-only fast path has one blind spot, and it is deliberate:
// EnsureWorkspaceBootstrap runs on EVERY command, so the steady-state check is
// pure stat and reads content only once size or mtime has moved. A content change
// that leaves BOTH identical is therefore not detected.
//
// This test pins that limit instead of leaving it to a filesystem's clock — the
// arrangement it builds by hand is exactly what a coarse-stamped filesystem
// produced by accident, which is what made TestEvalInstall_UVLockChangeIsStale
// pass locally and fail on CI. Writing it down means the trade is reviewed rather
// than rediscovered.
//
// If the fast path ever hashes unconditionally, this test must be deleted, not
// adjusted: its assertion is the cost of the fast path, not a guarantee.
func TestEvalInstall_SameSizeAndMtimeContentChangeIsNotDetected(t *testing.T) {
	ws := t.TempDir()
	p := writeLock(t, ws, "uv.lock", "version = 1\n")
	declareInstallInputs(t, ws, "uv.lock")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	writeLock(t, ws, "uv.lock", "version = 2\n")
	stamp := recorded.ModTime()
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != recorded.Size() || after.ModTime().UnixNano() != recorded.ModTime().UnixNano() {
		t.Fatalf("fixture did not reproduce an identical size and mtime: size %d->%d, mtime %d->%d",
			recorded.Size(), after.Size(), recorded.ModTime().UnixNano(), after.ModTime().UnixNano())
	}

	stale, repair := evalInstall(ws)
	if stale || repair != nil {
		t.Errorf("the fast path grew a content read: stale=%v repair=%v — if that is intended, delete this test",
			stale, repair != nil)
	}
}

// The Go extension declares go.work and not its checksum files, which ordinary
// builds update, so a checksum change leaves the install current.
func TestEvalInstall_GoChecksumChangesAreNotStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	writeLock(t, ws, "go.sum", "example.com/dep v1.0.0 h1:old\n")
	writeLock(t, ws, "go.work.sum", "example.com/workdep v1.0.0/go.mod h1:old\n")
	declareInstallInputs(t, ws, "go.work")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}

	writeLock(t, ws, "go.sum", "example.com/dep v1.0.0 h1:new\n")
	writeLock(t, ws, "go.work.sum", "example.com/workdep v1.0.0/go.mod h1:new\n")

	stale, repair := evalInstall(ws)
	if stale {
		t.Error("Go checksum outputs must not force auto-install; Go commands maintain them during normal builds")
	}
	if repair != nil {
		t.Error("ignored Go checksum outputs should not require install-state repair")
	}
}

func TestEvalInstall_MtimeOnlyChangeIsNotStale(t *testing.T) {
	ws := t.TempDir()
	p := writeLock(t, ws, "go.work", "go 1.23\n")
	declareInstallInputs(t, ws, "go.work")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	// Simulate a git pull / branch switch: same content, moved mtime.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	stale, repair := evalInstall(ws)
	if stale {
		t.Error("identical content with a moved mtime must NOT be stale (no spurious reinstall)")
	}
	if repair == nil {
		t.Fatal("a moved mtime with identical content should yield a fast-path repair marker")
	}
	if repair.LockFiles[0].ModTime != future.UnixNano() {
		t.Errorf("repair marker should record the new mtime: got %d want %d", repair.LockFiles[0].ModTime, future.UnixNano())
	}
}

func TestEvalInstall_LockFileAddedIsStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	declareInstallInputs(t, ws, "go.work")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	writeLock(t, ws, "go.work", "go 1.23\n")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a newly added lock file must be stale (workspace gained a language)")
	}
}

func TestEvalInstall_LockFileRemovedIsStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	p := writeLock(t, ws, "go.work", "go 1.23\n")
	declareInstallInputs(t, ws, "go.work")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a removed lock file must be stale")
	}
}

func TestEvalInstall_VersionMismatchIsStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	if err := writeInstallStateRaw(ws, &InstallState{Version: installStateVersion + 1}); err != nil {
		t.Fatal(err)
	}
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a marker from a newer schema version must be treated as stale")
	}
}

func TestEvalInstall_NoLockFilesRoundTrips(t *testing.T) {
	ws := t.TempDir() // no lock files at all
	if stale, _ := evalInstall(ws); !stale {
		t.Error("no marker must be stale even with no lock files")
	}
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	if stale, repair := evalInstall(ws); stale || repair != nil {
		t.Error("after writing, an empty-lock workspace must be current")
	}
}
