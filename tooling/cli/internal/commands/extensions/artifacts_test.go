package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

func TestLockEntryForMergesSameVersionPlatformDigests(t *testing.T) {
	prior := lockfile.LockEntry{
		Version: "1.0.0",
		Integrities: map[string]string{
			"darwin/arm64": "machash",
		},
		Source: "https://registry/download?channel=1.0.0",
	}
	result := &artifactInstallOutcome{
		Version:      "1.0.0",
		Integrity:    "linuxhash",
		Integrities:  map[string]string{"linux/amd64": "linuxhash"},
		ManifestHash: "manifesthash",
		Source:       "https://registry/download?channel=1.0.0",
	}

	got := lockEntryFor(prior, true, result)

	if got.IntegrityFor("darwin", "arm64") != "machash" {
		t.Fatalf("prior platform digest was not preserved: %+v", got.Integrities)
	}
	if got.IntegrityFor("linux", "amd64") != "linuxhash" {
		t.Fatalf("current platform digest was not recorded: %+v", got.Integrities)
	}
}

// A same-version reinstall must not erase the lock-v2 task-contract record —
// erasing it would flap the `migrate vnext --check` CI gate after every
// install. A version bump resets it
// to "not recorded" on purpose: the new manifest may declare a different
// contract, and --check prompting a re-record is the gate working.
func TestLockEntryForTaskContractRecord(t *testing.T) {
	prior := lockfile.LockEntry{Version: "1.0.0", TaskContract: 3}

	sameVersion := lockEntryFor(prior, true, &artifactInstallOutcome{Version: "1.0.0"})
	if sameVersion.TaskContract != 3 {
		t.Fatalf("same-version reinstall erased the task-contract record: %+v", sameVersion)
	}

	bumped := lockEntryFor(prior, true, &artifactInstallOutcome{Version: "2.0.0"})
	if bumped.TaskContract != 0 {
		t.Fatalf("version bump carried a stale task-contract record: %+v", bumped)
	}

	fresh := lockEntryFor(lockfile.LockEntry{}, false, &artifactInstallOutcome{Version: "1.0.0"})
	if fresh.TaskContract != 0 {
		t.Fatalf("fresh install invented a task-contract record: %+v", fresh)
	}
}

func TestLockEntryForDropsPriorPlatformDigestsOnVersionChange(t *testing.T) {
	prior := lockfile.LockEntry{
		Version: "1.0.0",
		Integrities: map[string]string{
			"darwin/arm64": "old-machash",
		},
		Source: "https://registry/download?channel=1.0.0",
	}
	result := &artifactInstallOutcome{
		Version:      "2.0.0",
		Integrity:    "new-linuxhash",
		Integrities:  map[string]string{"linux/amd64": "new-linuxhash"},
		ManifestHash: "new-manifesthash",
		Source:       "https://registry/download?channel=2.0.0",
	}

	got := lockEntryFor(prior, true, result)

	if got.IntegrityFor("darwin", "arm64") != "" {
		t.Fatalf("prior version digest leaked into updated lock: %+v", got.Integrities)
	}
	if got.IntegrityFor("linux", "amd64") != "new-linuxhash" {
		t.Fatalf("new platform digest was not recorded: %+v", got.Integrities)
	}
	if got.Source != result.Source {
		t.Fatalf("Source = %q, want %q", got.Source, result.Source)
	}
}

// --- implicit-install lock-write suppression ---

func TestIsImplicitInstall(t *testing.T) {
	if shared.IsImplicitInstall(context.Background()) {
		t.Error("a plain context must not be marked implicit")
	}
	if !shared.IsImplicitInstall(shared.WithImplicitInstall(context.Background())) {
		t.Error("withImplicitInstall must mark the context")
	}
}

// fakeInstallOps returns an artifactOps that installs one configured artifact
// via a stub — no installer, no network — so installArtifacts' lock-write
// decision can be exercised directly.
func fakeInstallOps(name, constraint string, result *artifactInstallOutcome) artifactOps {
	return artifactOps{
		label:      "extension",
		plural:     "extensions",
		parseArg:   parseExtensionArg,
		configMap:  func(*wsproto.Config) map[string]string { return map[string]string{name: constraint} },
		lockGet:    func(lf *lockfile.LockFile, n string) (lockfile.LockEntry, bool) { return lf.GetExtension(n) },
		lockSet:    func(lf *lockfile.LockFile, n string, e lockfile.LockEntry) { lf.SetExtension(n, e) },
		lockRemove: func(lf *lockfile.LockFile, n string) { lf.RemoveExtension(n) },
		install: func(context.Context, string, string, *lockfile.LockEntry) (*artifactInstallOutcome, error) {
			return result, nil
		},
	}
}

// silenceInstallArtifacts runs installArtifacts with its human progress stream
// discarded so it does not clutter test logs. It passes io.Discard rather than
// swapping os.Stdout: the writer is a parameter now, and a
// test that still reassigned the process stdout would be pinning the very thing
// that change deleted.
func silenceInstallArtifacts(t *testing.T, ctx context.Context, ws string, ops artifactOps) {
	t.Helper()
	if err := installArtifacts(ctx, ws, &wsproto.Config{}, nil, ops, "", io.Discard); err != nil {
		t.Fatalf("installArtifacts: %v", err)
	}
}

func TestInstallArtifactsParallelWorkersCommitAndReportInStableOrder(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "parallel-artifacts", "parallel-restoration-commits-results-in-stable-order")

	names := []string{"foxtrot", "bravo", "echo", "alpha", "delta", "charlie"}
	configured := make(map[string]string, len(names))
	for _, name := range names {
		configured[name] = "1.0.0"
	}

	started := make(chan struct{}, len(names))
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	ops := artifactOps{
		label:     "extension",
		plural:    "extensions",
		parseArg:  parseExtensionArg,
		configMap: func(*wsproto.Config) map[string]string { return configured },
		lockGet:   func(lf *lockfile.LockFile, name string) (lockfile.LockEntry, bool) { return lf.GetExtension(name) },
		lockSet:   func(lf *lockfile.LockFile, name string, entry lockfile.LockEntry) { lf.SetExtension(name, entry) },
		install: func(_ context.Context, name, _ string, _ *lockfile.LockEntry) (*artifactInstallOutcome, error) {
			now := active.Add(1)
			for seen := maximum.Load(); now > seen && !maximum.CompareAndSwap(seen, now); seen = maximum.Load() {
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return &artifactInstallOutcome{Version: "1.0.0", Source: "registry/" + name}, nil
		},
	}

	ws := t.TempDir()
	var reported []string
	var callbackErr error
	done := make(chan error, 1)
	go func() {
		done <- installArtifactsWithOptions(context.Background(), ws, &wsproto.Config{}, nil, ops, InstallOptions{
			OnAction: func(action InstallAction) {
				lf, err := lockfile.ReadLockFile(ws)
				if err != nil {
					callbackErr = err
					return
				}
				if _, ok := lf.GetExtension(action.Name); !ok {
					callbackErr = fmt.Errorf("callback ran before %s was committed to the lock", action.Name)
					return
				}
				reported = append(reported, action.Name)
			},
		})
	}()

	for range maxParallelArtifactInstalls {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("artifact installs did not overlap up to the configured bound")
		}
	}
	if got := maximum.Load(); got != maxParallelArtifactInstalls {
		t.Fatalf("maximum concurrent installs = %d, want %d", got, maxParallelArtifactInstalls)
	}
	if len(reported) != 0 {
		t.Fatalf("reported before workers finished: %v", reported)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}
	want := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	if fmt.Sprint(reported) != fmt.Sprint(want) {
		t.Fatalf("reported order = %v, want %v", reported, want)
	}
}

// A committed lock listing only darwin/arm64; the "current host" resolves a new
// linux/amd64 digest that would otherwise be appended on install.
const committedDarwinOnlyLock = `{
  "version": 2,
  "extensions": {
    "@fake/ext": {
      "version": "1.0.0",
      "integrities": {
        "darwin/arm64": "machash"
      }
    }
  },
  "templates": {}
}
`

func newHostResult() *artifactInstallOutcome {
	return &artifactInstallOutcome{
		Version:     "1.0.0",
		Integrity:   "linuxhash",
		Integrities: map[string]string{"linux/amd64": "linuxhash"},
	}
}

func TestInstallArtifacts_ImplicitInstallDoesNotWriteLock(t *testing.T) {
	ws := t.TempDir()
	lockPath := sharedtest.WriteLock(t, ws, "putnami.lock.json", committedDarwinOnlyLock)
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	ops := fakeInstallOps("@fake/ext", "1.0.0", newHostResult())
	silenceInstallArtifacts(t, shared.WithImplicitInstall(context.Background()), ws, ops)

	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("implicit install rewrote the committed lock:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	lf, _ := lockfile.ReadLockFile(ws)
	if e, _ := lf.GetExtension("@fake/ext"); e.IntegrityFor("linux", "amd64") != "" {
		t.Errorf("implicit install laundered the current host's linux/amd64 digest into the committed lock")
	}
}

// An explicit install that resolved nothing new leaves the committed lock
// byte for byte as it is, even when this host verified a digest the lock does
// not list and downloaded through another registry URL. A hosted
// runner on linux/amd64 used to write both into a darwin/arm64 lock, and
// `--impacted` then selected every project that reads the lock.
func TestInstallArtifacts_ExplicitInstallDoesNotWriteAMachineLocalRefresh(t *testing.T) {
	ws := t.TempDir()
	lockPath := sharedtest.WriteLock(t, ws, "putnami.lock.json", committedDarwinOnlyLock)
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	result := newHostResult()
	result.Source = "http://127.0.0.1:41000/put/fake/ext/download?channel=1.0.0"
	ops := fakeInstallOps("@fake/ext", "1.0.0", result)
	silenceInstallArtifacts(t, context.Background(), ws, ops)

	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("explicit install rewrote the committed lock for this host's digest alone:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	afterInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Error("an unchanged lock was replaced (new inode)")
	}
}

// When a requested fact did change, the lock is written, and the digests this
// host verified ride along — but a version that did not move keeps its
// committed source, whatever URL this host downloaded it from.
func TestInstallArtifacts_ExplicitInstallRecordsTheHostDigestBesideARequestedChange(t *testing.T) {
	ws := t.TempDir()
	sharedtest.WriteLock(t, ws, "putnami.lock.json", `{
  "version": 2,
  "extensions": {
    "@fake/ext": {
      "version": "1.0.0",
      "integrities": {
        "darwin/arm64": "machash"
      },
      "source": "https://registry/@fake/ext/download?channel=1.0.0"
    }
  },
  "templates": {}
}
`)

	results := map[string]*artifactInstallOutcome{
		"@fake/ext": {
			Version: "1.0.0", Integrity: "linuxhash",
			Integrities: map[string]string{"linux/amd64": "linuxhash"},
			Source:      "http://127.0.0.1:41000/put/fake/ext/download?channel=1.0.0",
		},
		// Newly configured: nothing in the lock yet, which is a requested change.
		"@fake/new": {
			Version: "2.0.0", Integrity: "newhash",
			Integrities: map[string]string{"linux/amd64": "newhash"},
			Source:      "https://registry/@fake/new/download?channel=2.0.0",
		},
	}
	ops := artifactOps{
		label:    "extension",
		plural:   "extensions",
		parseArg: parseExtensionArg,
		configMap: func(*wsproto.Config) map[string]string {
			return map[string]string{"@fake/ext": "1.0.0", "@fake/new": "2.0.0"}
		},
		lockGet: func(lf *lockfile.LockFile, n string) (lockfile.LockEntry, bool) { return lf.GetExtension(n) },
		lockSet: func(lf *lockfile.LockFile, n string, e lockfile.LockEntry) { lf.SetExtension(n, e) },
		install: func(_ context.Context, name, _ string, _ *lockfile.LockEntry) (*artifactInstallOutcome, error) {
			return results[name], nil
		},
		localPlatform: "linux/amd64",
	}
	silenceInstallArtifacts(t, context.Background(), ws, ops)

	existing := lockedExtension(t, ws, "@fake/ext")
	if got := existing.IntegrityFor("linux", "amd64"); got != "linuxhash" {
		t.Errorf("host digest = %q, want linuxhash recorded beside the requested change", got)
	}
	if got := existing.IntegrityFor("darwin", "arm64"); got != "machash" {
		t.Errorf("prior digest = %q, want machash kept", got)
	}
	if want := "https://registry/@fake/ext/download?channel=1.0.0"; existing.Source != want {
		t.Errorf("source of the unmoved version = %q, want the committed %q", existing.Source, want)
	}
	if added := lockedExtension(t, ws, "@fake/new"); added.Version != "2.0.0" || added.IntegrityFor("linux", "amd64") != "newhash" {
		t.Errorf("new entry = %+v, want 2.0.0 with its verified digest", added)
	}
}

// The predicate itself: only a same-version entry whose sole differences are
// digests the prior did not list, and the source, is a machine-local refresh.
func TestMachineLocalRefresh(t *testing.T) {
	prior := lockfile.LockEntry{
		Version: "1.0.0", ManifestHash: "manifest", TaskContract: 3,
		Integrities: map[string]string{"darwin/arm64": "machash"},
		Source:      "https://registry/download?channel=1.0.0",
	}
	with := func(edit func(*lockfile.LockEntry)) lockfile.LockEntry {
		entry := prior
		entry.Integrities = map[string]string{"darwin/arm64": "machash", "linux/amd64": "linuxhash"}
		entry.Integrity = "linuxhash"
		entry.Source = "http://127.0.0.1:41000/put/download?channel=1.0.0"
		if edit != nil {
			edit(&entry)
		}
		return entry
	}
	cases := []struct {
		name     string
		prior    lockfile.LockEntry
		hadPrior bool
		entry    lockfile.LockEntry
		want     bool
	}{
		{name: "added host digest and source", prior: prior, hadPrior: true, entry: with(nil), want: true},
		{name: "identical", prior: prior, hadPrior: true, entry: prior, want: true},
		{name: "no prior entry", prior: lockfile.LockEntry{}, hadPrior: false, entry: with(nil), want: false},
		{name: "moved version", prior: prior, hadPrior: true, entry: with(func(e *lockfile.LockEntry) { e.Version = "1.1.0" }), want: false},
		{name: "changed recorded digest", prior: prior, hadPrior: true, entry: with(func(e *lockfile.LockEntry) { e.Integrities["darwin/arm64"] = "other" }), want: false},
		{name: "changed manifest hash", prior: prior, hadPrior: true, entry: with(func(e *lockfile.LockEntry) { e.ManifestHash = "other" }), want: false},
		{name: "changed task contract", prior: prior, hadPrior: true, entry: with(func(e *lockfile.LockEntry) { e.TaskContract = 2 }), want: false},
		{name: "legacy scalar-only prior migrates", prior: lockfile.LockEntry{Version: "1.0.0", Integrity: "machash"}, hadPrior: true, entry: with(nil), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := machineLocalRefresh(tc.prior, tc.hadPrior, tc.entry); got != tc.want {
				t.Errorf("machineLocalRefresh = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestLockEntryForPreservesSourceOnCachedInstall(t *testing.T) {
	prior := lockfile.LockEntry{
		Version:      "1.0.0",
		ManifestHash: "manifesthash",
		Source:       "https://registry/download?channel=1.0.0",
		Integrities:  map[string]string{"darwin/arm64": "machash"},
	}
	result := &artifactInstallOutcome{
		Version:      "1.0.0",
		ManifestHash: "manifesthash",
		FromCache:    true,
	}

	got := lockEntryFor(prior, true, result)

	if got.Source != prior.Source {
		t.Fatalf("cached install Source = %q, want preserved %q", got.Source, prior.Source)
	}
	if got.IntegrityFor("darwin", "arm64") != "machash" {
		t.Fatalf("cached install dropped platform digest: %+v", got.Integrities)
	}
}

// --- cross-platform integrity carry-forward on a version bump ---

// updateRecorder is the stub half of an artifactOps for updateArtifacts: it
// "installs" a fixed outcome and records every cross-platform integrity
// resolution, so a test can assert both the resulting lock and the exact number
// of extra registry round-trips a version bump costs.
type updateRecorder struct {
	// calls records "name@version os/arch" in call order.
	calls []string
	// advertise maps "os/arch" to the digest the registry advertises at the new
	// version; a platform absent from the map is unresolvable.
	advertise map[string]string
}

func (r *updateRecorder) ops(name, localPlatform string, result *artifactInstallOutcome) artifactOps {
	return artifactOps{
		label:      "extension",
		plural:     "extensions",
		parseArg:   parseExtensionArg,
		configMap:  func(*wsproto.Config) map[string]string { return map[string]string{name: "latest"} },
		lockGet:    func(lf *lockfile.LockFile, n string) (lockfile.LockEntry, bool) { return lf.GetExtension(n) },
		lockSet:    func(lf *lockfile.LockFile, n string, e lockfile.LockEntry) { lf.SetExtension(n, e) },
		lockRemove: func(lf *lockfile.LockFile, n string) { lf.RemoveExtension(n) },
		install: func(context.Context, string, string, *lockfile.LockEntry) (*artifactInstallOutcome, error) {
			return result, nil
		},
		localPlatform: localPlatform,
		resolvePlatformIntegrity: func(_ context.Context, n, version, goos, goarch string) (string, error) {
			platform := lockfile.PlatformKey(goos, goarch)
			r.calls = append(r.calls, fmt.Sprintf("%s@%s %s", n, version, platform))
			if digest, ok := r.advertise[platform]; ok {
				return digest, nil
			}
			return "", fmt.Errorf("HTTP 404")
		},
	}
}

// runUpdateArtifacts runs updateArtifacts with stdout discarded, returning what
// it wrote to stderr. It fails the test if the update itself errors: a
// cross-platform resolution failure must never fail the upgrade.
func runUpdateArtifacts(t *testing.T, ws string, ops artifactOps, outputFormat string) string {
	t.Helper()
	var runErr error
	// updateArtifacts still renders on the process stdout (only the INSTALL path
	// takes a writer), so this one captures it.
	stderr := sharedtest.CaptureStderr(t, func() {
		_, _ = sharedtest.CaptureStdout(t, func() error {
			runErr = updateArtifacts(context.Background(), ws, &wsproto.Config{}, nil, ops, ArtifactUpdateOptions{}, outputFormat)
			return nil
		})
	})
	if runErr != nil {
		t.Fatalf("updateArtifacts: %v", runErr)
	}
	return stderr
}

// A committed lock pinning two platforms at 1.0.0 — the shape a shared repo
// accumulates and that a framework bump used to shrink to one platform.
const committedTwoPlatformLock = `{
  "version": 2,
  "extensions": {
    "@fake/ext": {
      "version": "1.0.0",
      "integrities": {
        "darwin/arm64": "old-machash",
        "linux/amd64": "old-linuxhash"
      },
      "manifestHash": "old-manifesthash",
      "source": "https://registry/@fake/ext/download?channel=1.0.0"
    }
  },
  "templates": {}
}
`

func lockedExtension(t *testing.T, ws, name string) lockfile.LockEntry {
	t.Helper()
	lf, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	entry, ok := lf.GetExtension(name)
	if !ok {
		t.Fatalf("lock lost the %s entry", name)
	}
	return entry
}

// TestUpdateArtifacts_VersionBumpKeepsForeignPlatformsAtNewVersion is the
// regression test: a bump must keep every platform the lock already
// recorded, each re-resolved AT THE NEW VERSION — never the prior version's
// digest, which would hard-fail installs on that platform.
func TestUpdateArtifacts_VersionBumpKeepsForeignPlatformsAtNewVersion(t *testing.T) {
	ws := t.TempDir()
	sharedtest.WriteLock(t, ws, "putnami.lock.json", committedTwoPlatformLock)

	rec := &updateRecorder{advertise: map[string]string{"linux/amd64": "new-linuxhash"}}
	ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
		Version:      "2.0.0",
		Integrity:    "new-machash",
		Integrities:  map[string]string{"darwin/arm64": "new-machash"},
		ManifestHash: "new-manifesthash",
		Source:       "https://registry/@fake/ext/download?channel=2.0.0",
	})

	if stderr := runUpdateArtifacts(t, ws, ops, ""); strings.Contains(stderr, "!") {
		t.Errorf("unexpected warning on the happy path: %q", stderr)
	}

	entry := lockedExtension(t, ws, "@fake/ext")
	if got := entry.IntegrityFor("darwin", "arm64"); got != "new-machash" {
		t.Errorf("host digest = %q, want the freshly hashed new-machash", got)
	}
	if got := entry.IntegrityFor("linux", "amd64"); got != "new-linuxhash" {
		t.Errorf("foreign digest = %q, want new-linuxhash (the prior linux/amd64 entry was dropped)", got)
	}
	if entry.IntegrityFor("linux", "amd64") == "old-linuxhash" {
		t.Error("carried the prior version's digest forward — it would fail integrity on linux/amd64")
	}
	want := []string{"@fake/ext@2.0.0 linux/amd64"}
	if fmt.Sprint(rec.calls) != fmt.Sprint(want) {
		t.Errorf("resolutions = %v, want exactly %v", rec.calls, want)
	}
}

// TestUpdateArtifacts_UnresolvableForeignPlatformWarnsAndSucceeds pins the
// fail-soft rule: an upgrade must never break because a foreign platform is
// unavailable at the new version.
func TestUpdateArtifacts_UnresolvableForeignPlatformWarnsAndSucceeds(t *testing.T) {
	ws := t.TempDir()
	lockPath := sharedtest.WriteLock(t, ws, "putnami.lock.json", committedTwoPlatformLock)

	rec := &updateRecorder{} // registry has nothing for linux/amd64 at 2.0.0
	ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
		Version:      "2.0.0",
		Integrity:    "new-machash",
		Integrities:  map[string]string{"darwin/arm64": "new-machash"},
		ManifestHash: "new-manifesthash",
	})

	stderr := runUpdateArtifacts(t, ws, ops, "")

	entry := lockedExtension(t, ws, "@fake/ext")
	if got := entry.IntegrityFor("darwin", "arm64"); got != "new-machash" {
		t.Errorf("host digest = %q, want new-machash: the platform that worked must still upgrade", got)
	}
	if got := entry.IntegrityFor("linux", "amd64"); got != "" {
		t.Errorf("unresolvable platform digest = %q, want it omitted", got)
	}
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "old-linuxhash") {
		t.Error("stale digest survived a failed re-resolution")
	}
	for _, want := range []string{"@fake/ext", "linux/amd64", "2.0.0"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("warning %q does not mention %q", stderr, want)
		}
	}
	if !strings.Contains(stderr, "  ! ") {
		t.Errorf("warning %q does not use the surrounding \"  ! \" style", stderr)
	}
}

// TestUpdateArtifacts_JSONLSuppressesCarryWarning keeps human-readable noise off
// the structured-output path.
func TestUpdateArtifacts_JSONLSuppressesCarryWarning(t *testing.T) {
	ws := t.TempDir()
	sharedtest.WriteLock(t, ws, "putnami.lock.json", committedTwoPlatformLock)

	rec := &updateRecorder{}
	ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
		Version:     "2.0.0",
		Integrity:   "new-machash",
		Integrities: map[string]string{"darwin/arm64": "new-machash"},
	})

	if stderr := runUpdateArtifacts(t, ws, ops, "jsonl"); stderr != "" {
		t.Errorf("jsonl run wrote human output to stderr: %q", stderr)
	}
}

// TestUpdateArtifacts_SameVersionRerunIsByteIdentical pins determinism: an
// unchanged version copies the prior map (no re-resolution, no network) and
// rewrites the same bytes every time.
func TestUpdateArtifacts_SameVersionRerunIsByteIdentical(t *testing.T) {
	ws := t.TempDir()
	lockPath := sharedtest.WriteLock(t, ws, "putnami.lock.json", committedTwoPlatformLock)

	rec := &updateRecorder{advertise: map[string]string{"linux/amd64": "must-not-be-used"}}
	ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
		Version:      "1.0.0",
		Integrity:    "old-machash",
		Integrities:  map[string]string{"darwin/arm64": "old-machash"},
		ManifestHash: "old-manifesthash",
		Source:       "https://registry/@fake/ext/download?channel=1.0.0",
	})

	runUpdateArtifacts(t, ws, ops, "")
	first, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	runUpdateArtifacts(t, ws, ops, "")
	second, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Errorf("re-running update on an unchanged version rewrote the lock:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if len(rec.calls) != 0 {
		t.Errorf("unchanged version cost %d extra registry round-trips: %v", len(rec.calls), rec.calls)
	}
	if got := lockedExtension(t, ws, "@fake/ext").IntegrityFor("linux", "amd64"); got != "old-linuxhash" {
		t.Errorf("same-version foreign digest = %q, want the copied old-linuxhash", got)
	}
}

// TestUpdateArtifacts_NoExtraRequestsWithoutForeignPlatforms guards constraint 8:
// the carry-forward may not add round-trips to the common cases.
func TestUpdateArtifacts_NoExtraRequestsWithoutForeignPlatforms(t *testing.T) {
	tests := []struct {
		name string
		lock string
	}{
		{"no prior entry", `{"version":2,"extensions":{},"templates":{}}` + "\n"},
		{"host-only prior", committedDarwinOnlyLock},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			sharedtest.WriteLock(t, ws, "putnami.lock.json", tc.lock)

			rec := &updateRecorder{}
			ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
				Version:     "2.0.0",
				Integrity:   "new-machash",
				Integrities: map[string]string{"darwin/arm64": "new-machash"},
			})

			runUpdateArtifacts(t, ws, ops, "")

			if len(rec.calls) != 0 {
				t.Errorf("cost %d extra registry round-trips: %v", len(rec.calls), rec.calls)
			}
		})
	}
}

// TestUpdateArtifacts_NeverResolvesHostPlatformFromRegistry keeps the host's
// digest on the strong ladder: it may only come from bytes this process
// downloaded, hashed and verified, never from a registry assertion. An install
// that could not verify its bytes reports no digest, and the lock must simply
// lose that platform.
func TestUpdateArtifacts_NeverResolvesHostPlatformFromRegistry(t *testing.T) {
	ws := t.TempDir()
	sharedtest.WriteLock(t, ws, "putnami.lock.json", committedTwoPlatformLock)

	rec := &updateRecorder{advertise: map[string]string{
		"darwin/arm64": "registry-asserted-machash",
		"linux/amd64":  "new-linuxhash",
	}}
	// An unverified (per-worktree) install: no digest for the host platform.
	ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
		Version:      "2.0.0",
		ManifestHash: "new-manifesthash",
	})

	runUpdateArtifacts(t, ws, ops, "")

	entry := lockedExtension(t, ws, "@fake/ext")
	if got := entry.IntegrityFor("darwin", "arm64"); got != "" {
		t.Errorf("host digest = %q, want it absent — it was never locally verified", got)
	}
	if got := entry.IntegrityFor("linux", "amd64"); got != "new-linuxhash" {
		t.Errorf("foreign digest = %q, want new-linuxhash", got)
	}
	want := []string{"@fake/ext@2.0.0 linux/amd64"}
	if fmt.Sprint(rec.calls) != fmt.Sprint(want) {
		t.Errorf("resolutions = %v, want exactly %v (the host platform is never asked for)", rec.calls, want)
	}
}

// TestUpdateArtifacts_MalformedPlatformKeyIsDroppedWithoutRequest keeps a
// hand-edited lock from turning into a bogus registry query.
func TestUpdateArtifacts_MalformedPlatformKeyIsDroppedWithoutRequest(t *testing.T) {
	ws := t.TempDir()
	sharedtest.WriteLock(t, ws, "putnami.lock.json", `{
  "version": 2,
  "extensions": {
    "@fake/ext": {
      "version": "1.0.0",
      "integrities": { "linux": "bogus" }
    }
  },
  "templates": {}
}
`)

	rec := &updateRecorder{}
	ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
		Version:     "2.0.0",
		Integrity:   "new-machash",
		Integrities: map[string]string{"darwin/arm64": "new-machash"},
	})

	stderr := runUpdateArtifacts(t, ws, ops, "")

	if len(rec.calls) != 0 {
		t.Errorf("malformed key reached the registry: %v", rec.calls)
	}
	if !strings.Contains(stderr, "malformed platform key") {
		t.Errorf("stderr = %q, want a malformed-key warning", stderr)
	}
	if _, ok := lockedExtension(t, ws, "@fake/ext").Integrities["linux"]; ok {
		t.Error("malformed platform key survived into the refreshed lock")
	}
}

// TestArtifactOps_WireCrossPlatformResolution guards the seam itself: both kinds
// must carry a resolver and their own platform key, or a version bump silently
// goes back to dropping platforms.
func TestArtifactOps_WireCrossPlatformResolution(t *testing.T) {
	tests := []struct {
		name         string
		ops          artifactOps
		wantPlatform string
	}{
		{"extensions", extensionOps(t.TempDir()), lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)},
		{"templates", templateOps(t.TempDir()), lockfile.PlatformKey("linux", "x64")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ops.resolvePlatformIntegrity == nil {
				t.Error("resolvePlatformIntegrity is not wired")
			}
			if tc.ops.localPlatform != tc.wantPlatform {
				t.Errorf("localPlatform = %q, want %q", tc.ops.localPlatform, tc.wantPlatform)
			}
		})
	}
}

// --- the install path advances versions too ---

// runInstallArtifacts runs installArtifacts with stdout discarded, returning
// what it wrote to stderr. Like the update helper it fails the test if the
// install itself errors: a cross-platform resolution failure must never fail an
// install either.
func runInstallArtifacts(t *testing.T, ctx context.Context, ws string, ops artifactOps, args []string) string {
	t.Helper()
	var runErr error
	stderr := sharedtest.CaptureStderr(t, func() {
		runErr = installArtifacts(ctx, ws, &wsproto.Config{}, args, ops, "", io.Discard)
	})
	if runErr != nil {
		t.Fatalf("installArtifacts: %v", runErr)
	}
	return stderr
}

// TestInstallArtifacts_VersionAdvancingPathsKeepForeignPlatforms covers the
// half of the fix that does NOT go through updateArtifacts. `--latest` and an
// explicit `name@version` both pass a nil lock entry, so they advance the
// version and start the same fresh per-platform map `update` does — without the
// carry-forward they recreate the exact cross-platform lock shrink this fix
// exists to prevent.
func TestInstallArtifacts_VersionAdvancingPathsKeepForeignPlatforms(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"latest", []string{"--latest"}},
		{"explicit version", []string{"@fake/ext@2.0.0"}},
		{"latest for one artifact", []string{"@fake/ext", "--latest"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			sharedtest.WriteLock(t, ws, "putnami.lock.json", committedTwoPlatformLock)

			rec := &updateRecorder{advertise: map[string]string{"linux/amd64": "new-linuxhash"}}
			ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
				Version:      "2.0.0",
				Integrity:    "new-machash",
				Integrities:  map[string]string{"darwin/arm64": "new-machash"},
				ManifestHash: "new-manifesthash",
				Source:       "https://registry/@fake/ext/download?channel=2.0.0",
			})

			if stderr := runInstallArtifacts(t, context.Background(), ws, ops, tc.args); strings.Contains(stderr, "!") {
				t.Errorf("unexpected warning on the happy path: %q", stderr)
			}

			entry := lockedExtension(t, ws, "@fake/ext")
			if got := entry.IntegrityFor("darwin", "arm64"); got != "new-machash" {
				t.Errorf("host digest = %q, want new-machash", got)
			}
			if got := entry.IntegrityFor("linux", "amd64"); got != "new-linuxhash" {
				t.Errorf("foreign digest = %q, want new-linuxhash — the install path dropped linux/amd64", got)
			}
			want := []string{"@fake/ext@2.0.0 linux/amd64"}
			if fmt.Sprint(rec.calls) != fmt.Sprint(want) {
				t.Errorf("resolutions = %v, want exactly %v", rec.calls, want)
			}
		})
	}
}

// TestInstallArtifacts_LockPinnedInstallIsCrossPlatformByteStable pins both the
// cost and reproducibility rules: a bare `putnami install` restores the locked
// version, copies the complete platform map without a registry request, and
// must not add a host-dependent scalar when Linux consumes a macOS-authored
// lock (or vice versa).
func TestInstallArtifacts_LockPinnedInstallIsCrossPlatformByteStable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		localPlatform string
		localDigest   string
	}{
		{"linux after macOS", "linux/amd64", "old-linuxhash"},
		{"macOS after linux", "darwin/arm64", "old-machash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			lockPath := sharedtest.WriteLock(t, ws, "putnami.lock.json", committedTwoPlatformLock)
			beforeInfo, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}

			rec := &updateRecorder{advertise: map[string]string{"unused/platform": "must-not-be-used"}}
			ops := rec.ops("@fake/ext", tc.localPlatform, &artifactInstallOutcome{
				Version:      "1.0.0",
				Integrity:    tc.localDigest,
				Integrities:  map[string]string{tc.localPlatform: tc.localDigest},
				ManifestHash: "old-manifesthash",
				Source:       "https://registry/@fake/ext/download?channel=1.0.0",
			})

			runInstallArtifacts(t, context.Background(), ws, ops, nil)
			after, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}

			if string(after) != string(before) {
				t.Errorf("cross-platform lock-pinned install rewrote the lock:\n--- before ---\n%s\n--- after ---\n%s", before, after)
			}
			afterInfo, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(beforeInfo, afterInfo) {
				t.Error("byte-identical lock-pinned install replaced the lock inode")
			}
			if len(rec.calls) != 0 {
				t.Errorf("a lock-pinned install made %d registry round-trips, want 0: %v", len(rec.calls), rec.calls)
			}
			entry := lockedExtension(t, ws, "@fake/ext")
			if got := entry.IntegrityFor("darwin", "arm64"); got != "old-machash" {
				t.Errorf("darwin/arm64 digest = %q, want old-machash", got)
			}
			if got := entry.IntegrityFor("linux", "amd64"); got != "old-linuxhash" {
				t.Errorf("linux/amd64 digest = %q, want old-linuxhash", got)
			}
		})
	}
}

// TestInstallArtifacts_ImplicitBootstrapMakesNoRequests guards this: an
// implicit first-use bootstrap discards its lock write, so it must not spend
// cross-platform round-trips producing a result that is thrown away — nor dirty
// the committed lock on a fresh worktree.
func TestInstallArtifacts_ImplicitBootstrapMakesNoRequests(t *testing.T) {
	ws := t.TempDir()
	lockPath := sharedtest.WriteLock(t, ws, "putnami.lock.json", committedTwoPlatformLock)
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	rec := &updateRecorder{advertise: map[string]string{"linux/amd64": "new-linuxhash"}}
	ops := rec.ops("@fake/ext", "darwin/arm64", &artifactInstallOutcome{
		Version:      "2.0.0",
		Integrity:    "new-machash",
		Integrities:  map[string]string{"darwin/arm64": "new-machash"},
		ManifestHash: "new-manifesthash",
		Source:       "https://registry/@fake/ext/download?channel=2.0.0",
	})

	runInstallArtifacts(t, shared.WithImplicitInstall(context.Background()), ws, ops, []string{"--latest"})

	if len(rec.calls) != 0 {
		t.Errorf("implicit bootstrap made %d registry round-trips, want 0: %v", len(rec.calls), rec.calls)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("implicit bootstrap dirtied the committed lock:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// --- removal under an unreadable lock ---

// legacyV1Lock is a lock in the format the vNext floor no longer reads. Every
// ReadLockFile over it returns an *OutdatedVersionError.
const legacyV1Lock = `{
  "version": 1,
  "extensions": {
    "@fake/ext": {
      "version": "1.0.0",
      "integrity": "sha256-old"
    }
  },
  "templates": {}
}
`

// TestRemoveArtifact_UnreadableLockRemovesNothing pins removal's atomicity: the
// lock read is checked BEFORE any deletion, so a lock this CLI cannot read
// aborts the command whole rather than half. Before B6r the read error was
// swallowed, which on a v1 lock skipped the lock edit, deleted the installed
// bytes anyway, and printed "✓ Removed" — leaving the artifact gone from disk
// and still pinned in the lock.
func TestRemoveArtifact_UnreadableLockRemovesNothing(t *testing.T) {
	ws := t.TempDir()
	lockPath := sharedtest.WriteLock(t, ws, "putnami.lock.json", legacyV1Lock)
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	removed := 0
	ops := artifactOps{
		label:      "extension",
		plural:     "extensions",
		parseArg:   parseExtensionArg,
		lockRemove: func(lf *lockfile.LockFile, n string) { lf.RemoveExtension(n) },
		remove:     func(string) error { removed++; return nil },
	}

	err = removeArtifact(ws, []string{"@fake/ext"}, ops)
	if err == nil {
		t.Fatal("removeArtifact over a v1 lock returned nil; the half-removal it hides is the bug")
	}
	var outdated *lockfile.OutdatedVersionError
	if !errors.As(err, &outdated) {
		t.Fatalf("removeArtifact error = %v, want an *OutdatedVersionError", err)
	}
	if !strings.Contains(err.Error(), "putnami migrate vnext --apply") {
		t.Errorf("error does not name the remedy: %v", err)
	}
	if removed != 0 {
		t.Errorf("ops.remove was called %d time(s); nothing may be deleted before the lock read succeeds", removed)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the unreadable lock was rewritten:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
