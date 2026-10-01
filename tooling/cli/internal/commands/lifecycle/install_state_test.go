package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// declareAdapterInputs writes the workspace index entries the install-state
// fingerprint reads.
//
// The fingerprint is core's own lock plus the
// ROOT-level `workspace.inputs` the extensions declare, and those declarations
// are read back from the persisted index rather than by discovering extensions
// (EnsureWorkspaceBootstrap runs on every command; discovery there would be the
// most expensive thing a no-op command does). A workspace with no index yet
// resolves to core's lock alone, which is the correct answer for a fresh
// checkout: nothing has been installed.
func declareAdapterInputs(t *testing.T, ws string, inputs ...string) {
	t.Helper()
	entries := make([]map[string]any, 0, len(inputs))
	for _, input := range inputs {
		entries = append(entries, map[string]any{"path": input, "digest": "absent"})
	}
	index := map[string]any{
		"version":        1,
		"probeDigest":    "wp1:test",
		"identityDigest": "wsid1:test",
		"providers": []map[string]any{{
			"extension": "@fixture/adapter",
			"digest":    "wp1:test",
			"result":    map[string]any{"version": 1, "extension": "@fixture/adapter"},
			"inputs":    entries,
		}},
	}
	encoded, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".putnami", "workspace-index.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
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

// A lock file no adapter declares — and that is not core's own — is invisible
// to the fingerprint. That is the layering, stated as a test: core stopped
// carrying a hard-coded table of language filenames, so a bun.lock in a
// workspace with no TypeScript extension changes nothing about whether the
// installed state is current.
func TestEvalInstall_UndeclaredLockFileIsNotAnInput(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	writeLock(t, ws, "bun.lock", "nobody declared me")
	if stale, _ := evalInstall(ws); stale {
		t.Error("an undeclared root file forced a re-install; only core's lock and adapter-declared inputs count")
	}
}

// And declaring it is what brings it back, without core learning the filename.
func TestEvalInstall_DeclaredLockFileBecomesAnInput(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	writeLock(t, ws, "bun.lock", "v1")
	declareAdapterInputs(t, ws, "bun.lock")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	writeLock(t, ws, "bun.lock", "v2-longer")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a declared adapter input changed and the install state stayed current")
	}
}

// PROBE inputs and INSTALL inputs answer different questions, and conflating
// them made a repo-root `biome.json` edit trigger a full workspace re-install.
// An adapter declares `workspace.inputs` to say what decides its probe
// ANSWER; the first-use marker asks whether the installed TREE is still current.
// Only the lock-shaped intersection counts.
func TestEvalInstall_NonLockRootInputsAreNotInstallInputs(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	writeLock(t, ws, "biome.json", `{"formatter":{"indentWidth":2}}`)
	writeLock(t, ws, "bun.lock", "v1")
	declareAdapterInputs(t, ws, "biome.json", "bunfig.toml", "bun.lock", "package.json",
		"tsconfig.base.json", "go.mod")

	got := workspaceInstallStateInputs(ws)
	want := []string{"bun.lock", "putnami.lock.json"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("inputs = %v, want %v — only lock-shaped declarations may enlist", got, want)
	}

	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	writeLock(t, ws, "biome.json", `{"formatter":{"indentWidth":4}}`)
	if stale, repair := evalInstall(ws); stale || repair != nil {
		t.Errorf("editing a root probe input forced a re-install (stale=%v repair=%v)", stale, repair != nil)
	}
	writeLock(t, ws, "bun.lock", "v2-longer")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("a declared LOCK changed and the install state stayed current")
	}
}

// Per-PROJECT manifests are metadata inputs, not install inputs: they invalidate
// the workspace probe, not the installed tree. Recording them here would make
// every project-level edit re-run the first-use install check.
func TestEvalInstall_OnlyRootLevelInputsCount(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	declareAdapterInputs(t, ws, "bun.lock", "apps/web/package.json", "*.lock")
	if got := workspaceInstallStateInputs(ws); len(got) != 2 || got[0] != "bun.lock" || got[1] != "putnami.lock.json" {
		t.Errorf("inputs = %v, want core's lock plus the root-level declaration only", got)
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
	declareAdapterInputs(t, ws, "bun.lock")
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
	declareAdapterInputs(t, ws, "bun.lock")
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	writeLock(t, ws, "bun.lock", "v2-different-length")
	if stale, _ := evalInstall(ws); !stale {
		t.Error("changed lock content must be stale")
	}
}

// uv.lock reaches the fingerprint the same way every other language lock does:
// the Python adapter declares it and core accepts the declaration. What this test
// owns is that plumbing, not the change-detection mechanism — TestEvalInstall_
// ContentChangeIsStale and TestEvalInstall_MtimeOnlyChangeIsNotStale own that.
//
// The rewrite keeps the length a real `version = N` bump would keep, so it goes
// through rewriteLock: on a filesystem whose stamps are coarser than two
// back-to-back writes, a same-length rewrite is otherwise invisible and this test
// would assert nothing.
func TestEvalInstall_UVLockChangeIsStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "uv.lock", "version = 1\n")
	declareAdapterInputs(t, ws, "uv.lock")
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
	declareAdapterInputs(t, ws, "uv.lock")
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

func TestEvalInstall_GoChecksumChangesAreNotStale(t *testing.T) {
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	writeLock(t, ws, "go.sum", "example.com/dep v1.0.0 h1:old\n")
	writeLock(t, ws, "go.work.sum", "example.com/workdep v1.0.0/go.mod h1:old\n")
	declareAdapterInputs(t, ws, "go.mod", "go.work")
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
	declareAdapterInputs(t, ws, "go.work")
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
	declareAdapterInputs(t, ws, "go.work")
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
	declareAdapterInputs(t, ws, "go.work")
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
