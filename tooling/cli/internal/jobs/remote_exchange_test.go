package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/store"
)

// staleExchangeAge is comfortably past the default one-hour grace.
const staleExchangeAge = 2 * time.Hour

// storeGCGraceEnv is the store's PUTNAMI_STORE_GC_GRACE override, which the
// sweep reuses.
const storeGCGraceEnv = "PUTNAMI_STORE_GC_GRACE"

type exchangeOwnerState int

const (
	// ownerNone is a directory with no owner.lock: an older CLI that predates
	// the owner-lock protocol.
	ownerNone exchangeOwnerState = iota
	// ownerDead is an owner.lock nobody holds: the kernel dropped it when the
	// owning run died.
	ownerDead
	// ownerLive is an owner.lock held for the rest of the test, as a running
	// session holds it.
	ownerLive
)

// makeExchangeDir creates parent/name holding one blob, sets its owner lock to
// state, and backdates it by age. The mtime is set last, because creating the
// blob and the lock file both touch it.
func makeExchangeDir(t *testing.T, parent, name string, age time.Duration, state exchangeOwnerState) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(dir, "ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ab", "blob"), []byte("blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state != ownerNone {
		owner, err := flock.Acquire(filepath.Join(dir, exchangeOwnerLockName), true, true)
		if err != nil {
			t.Fatalf("take %s owner lock: %v", name, err)
		}
		if state == ownerDead {
			_ = owner.Release()
		} else {
			t.Cleanup(func() { _ = owner.Release() })
		}
	}
	backdate(t, dir, age)
	return dir
}

func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func requireExists(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s must survive (%s): %v", filepath.Base(path), why, err)
	}
}

func requireGone(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s must be deleted (%s); stat err = %v", filepath.Base(path), why, err)
	}
}

func exchangeDirsUnder(t *testing.T, parent string) []string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), exchangeDirPrefix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// requireOwnerLockHeld proves a live session holds dir's owner lock: another
// holder, which is what a concurrent run's sweep is, cannot take it.
func requireOwnerLockHeld(t *testing.T, dir string) {
	t.Helper()
	l, err := flock.Acquire(filepath.Join(dir, exchangeOwnerLockName), true, true)
	if err == nil {
		_ = l.Release()
		t.Fatalf("%s owner lock is free while its session lives; a sweep would delete it", filepath.Base(dir))
	}
	if !errors.Is(err, flock.ErrBusy) {
		t.Fatalf("%s owner lock: err = %v, want flock.ErrBusy", filepath.Base(dir), err)
	}
}

// neverStop is the stop check of a sweep nobody interrupts.
func neverStop() bool { return false }

// TestSweepStaleExchangeDirsDeletesOnlyProvablyDeadDirs pins both liveness
// signals: a directory goes only when it is older than the grace AND its owner
// lock is free. The live owner past the grace is the case age alone gets wrong.
// A directory with no owner.lock is swept too, because flock.Acquire creates
// the file: that is both an older CLI's directory and one whose deletion a
// killed process cut short.
func TestSweepStaleExchangeDirsDeletesOnlyProvablyDeadDirs(t *testing.T) {
	parent := t.TempDir()

	legacy := makeExchangeDir(t, parent, exchangeDirPrefix+"legacy", staleExchangeAge, ownerNone)
	crashed := makeExchangeDir(t, parent, exchangeDirPrefix+"crashed", staleExchangeAge, ownerDead)
	// A RemoveAll cut short: owner.lock is gone and one shard is already empty,
	// while another shard still holds a blob.
	interrupted := makeExchangeDir(t, parent, exchangeDirPrefix+"interrupted", staleExchangeAge, ownerDead)
	if err := os.Remove(filepath.Join(interrupted, exchangeOwnerLockName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(interrupted, "cd"), 0o755); err != nil {
		t.Fatal(err)
	}
	backdate(t, interrupted, staleExchangeAge)
	liveLong := makeExchangeDir(t, parent, exchangeDirPrefix+"live-long", staleExchangeAge, ownerLive)
	fresh := makeExchangeDir(t, parent, exchangeDirPrefix+"fresh", 0, ownerNone)
	unrelated := makeExchangeDir(t, parent, "blobs", staleExchangeAge, ownerNone)

	// Entries that only look like exchange directories by name.
	outside := makeExchangeDir(t, t.TempDir(), exchangeDirPrefix+"outside", staleExchangeAge, ownerNone)
	link := filepath.Join(parent, exchangeDirPrefix+"link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(parent, exchangeDirPrefix+"file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(t, notDir, staleExchangeAge)

	if got := sweepStaleExchangeDirs(parent, time.Now(), time.Hour, neverStop); got != 3 {
		t.Fatalf("sweep deleted %d directories, want 3 (legacy, crashed, interrupted)", got)
	}
	requireGone(t, legacy, "past the grace and never locked")
	requireGone(t, crashed, "past the grace and its owner's lock is free")
	requireGone(t, interrupted, "past the grace, and its owner.lock is recreated and taken")
	requireExists(t, liveLong, "its owner still holds the lock, however old the directory is")
	requireOwnerLockHeld(t, liveLong)
	requireExists(t, fresh, "younger than the grace")
	requireExists(t, unrelated, "not an exchange directory")
	requireExists(t, link, "a symlink, not a directory")
	requireExists(t, filepath.Join(outside, "ab", "blob"), "outside the parent, behind a symlink")
	requireExists(t, notDir, "not a directory")

	// A missing parent is not an error: there is nothing to sweep.
	if got := sweepStaleExchangeDirs(filepath.Join(parent, "missing"), time.Now(), time.Hour, neverStop); got != 0 {
		t.Fatalf("sweep of a missing parent deleted %d directories", got)
	}
}

// TestExchangeSweepGraceReusesStoreGraceWithFloor pins the grace to the store
// GC's (default and override), and the floor that keeps a zero override from
// exposing a directory between its creation and its owner's lock.
func TestExchangeSweepGraceReusesStoreGraceWithFloor(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{env: "", want: time.Hour},
		{env: "3h", want: 3 * time.Hour},
		{env: "0s", want: minExchangeSweepGrace},
	} {
		t.Setenv(storeGCGraceEnv, tc.env)
		if got := exchangeSweepGrace(); got != tc.want {
			t.Errorf("%s=%q: grace = %v, want %v", storeGCGraceEnv, tc.env, got, tc.want)
		}
	}
}

// startProviderSession loads a provider-backed cache over the in-process fake
// and starts its session. The run-marker lookup passes no cache manager, so the
// exchange parent is <wsRoot>/.putnami.
func startProviderSession(t *testing.T, wsRoot string) *RemoteCache {
	t.Helper()
	ext := fakeProviderExtension(t, map[string]string{"FAKE_MARKER_SHA": "cafef00d"})
	remote, _ := LoadRemoteCache(context.Background(), wsRoot, []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v", remote)
	}
	if _, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); !ok {
		t.Fatal("provider session did not start")
	}
	return remote
}

// TestProviderStartSweepsStaleExchangeDirs is the sweep acceptance test: a
// session start deletes a sibling a killed run left past the grace, and
// keeps a fresh one and a live one. The test waits for the sweep, as a run
// that outlives it does, because Close stops a sweep that is still running.
func TestProviderStartSweepsStaleExchangeDirs(t *testing.T) {
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	t.Setenv(storeGCGraceEnv, "")
	wsRoot := t.TempDir()
	parent := filepath.Join(wsRoot, ".putnami")

	stale := makeExchangeDir(t, parent, exchangeDirPrefix+"killed", staleExchangeAge, ownerNone)
	fresh := makeExchangeDir(t, parent, exchangeDirPrefix+"starting", 0, ownerNone)
	live := makeExchangeDir(t, parent, exchangeDirPrefix+"long-build", staleExchangeAge, ownerLive)

	remote := startProviderSession(t, wsRoot)
	own := remote.providerExchangeDir()
	if filepath.Dir(own) != parent || !strings.HasPrefix(filepath.Base(own), exchangeDirPrefix) {
		t.Fatalf("session exchange dir = %q, want a %s* directory under %q", own, exchangeDirPrefix, parent)
	}
	requireOwnerLockHeld(t, own)

	remote.provider.sweep.Wait()
	requireGone(t, stale, "a killed run's directory past the grace")
	requireExists(t, fresh, "younger than the grace")
	requireExists(t, live, "a concurrent run still holds its lock")
	requireExists(t, own, "the session's own directory is live")

	remote.Close()
	requireGone(t, own, "the session's own directory is deleted at shutdown")
}

// TestProviderExchangeDirOutlivesGraceWhileSessionRuns pins the case the issue
// warns about: a session running longer than the grace. A sweep that believes
// hours have passed still cannot take the live session's lock, and the lock is
// free again once the session has deleted its directory.
func TestProviderExchangeDirOutlivesGraceWhileSessionRuns(t *testing.T) {
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	wsRoot := t.TempDir()
	parent := filepath.Join(wsRoot, ".putnami")

	remote := startProviderSession(t, wsRoot)
	own := remote.providerExchangeDir()
	if got := sweepStaleExchangeDirs(parent, time.Now().Add(24*time.Hour), time.Hour, neverStop); got != 0 {
		t.Fatalf("a sweep deleted %d directories while the only one belongs to a live session", got)
	}
	requireExists(t, own, "its session is still running")

	remote.Close()
	requireGone(t, own, "the session's own directory is deleted at shutdown")
	if names := exchangeDirsUnder(t, parent); len(names) != 0 {
		t.Fatalf("exchange directories left after Close: %v", names)
	}
}

// TestProviderStartFailureLeavesNoExchangeDir pins the failure path: a provider
// that cannot start deletes the directory it was given and releases its lock,
// and the sweep that start launched still runs.
func TestProviderStartFailureLeavesNoExchangeDir(t *testing.T) {
	enableProviderRemoteCache(t)
	orig := spawnProviderSession
	spawnProviderSession = func(context.Context, cacheprovider.LaunchSpec, ...cacheprovider.Option) (*cacheprovider.Session, error) {
		return nil, errors.New("provider binary missing")
	}
	t.Cleanup(func() { spawnProviderSession = orig })
	wsRoot := t.TempDir()
	parent := filepath.Join(wsRoot, ".putnami")
	stale := makeExchangeDir(t, parent, exchangeDirPrefix+"killed", staleExchangeAge, ownerDead)

	ext := fakeProviderExtension(t, nil)
	remote, _ := LoadRemoteCache(context.Background(), wsRoot, []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v", remote)
	}
	if _, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); ok {
		t.Fatal("a provider that cannot start must not serve a run marker")
	}
	if dir := remote.providerExchangeDir(); dir != "" {
		t.Fatalf("a failed start must forget its exchange dir, got %q", dir)
	}
	remote.provider.sweep.Wait()
	remote.Close()

	requireGone(t, stale, "the sweep runs even when the provider cannot start")
	if names := exchangeDirsUnder(t, parent); len(names) != 0 {
		t.Fatalf("exchange directories left after a failed start: %v", names)
	}
}

// TestSweepStaleExchangeDirsStopsBetweenDirectories pins the stop: a sweep
// asked to stop before it starts deletes nothing, one asked to stop while it is
// on a directory finishes that directory only, and the next sweep reclaims
// what the stopped ones left.
func TestSweepStaleExchangeDirsStopsBetweenDirectories(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		makeExchangeDir(t, parent, exchangeDirPrefix+name, staleExchangeAge, ownerDead)
	}

	if got := sweepStaleExchangeDirs(parent, time.Now(), time.Hour, func() bool { return true }); got != 0 {
		t.Fatalf("a sweep stopped before it started deleted %d directories", got)
	}
	if names := exchangeDirsUnder(t, parent); len(names) != 3 {
		t.Fatalf("a sweep stopped before it started left %v, want all 3", names)
	}

	// The stop arrives while the sweep is on its first directory.
	checks := 0
	stopAfterFirst := func() bool {
		checks++
		return checks > 1
	}
	if got := sweepStaleExchangeDirs(parent, time.Now(), time.Hour, stopAfterFirst); got != 1 {
		t.Fatalf("a sweep stopped on its first directory deleted %d, want 1", got)
	}
	if names := exchangeDirsUnder(t, parent); len(names) != 2 {
		t.Fatalf("a sweep stopped on its first directory left %v, want 2", names)
	}

	if got := sweepStaleExchangeDirs(parent, time.Now(), time.Hour, neverStop); got != 2 {
		t.Fatalf("the next sweep deleted %d directories, want the 2 left", got)
	}
}

// TestProviderCloseStopsItsSweepBeforeWaiting pins shutdown's side: Close asks
// the sweep to stop, then waits. The sweep stand-in has endless work and ends
// only when asked, so a Close that waited first would wait for the stand-in's
// failure deadline and fail here.
func TestProviderCloseStopsItsSweepBeforeWaiting(t *testing.T) {
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	started := make(chan struct{})
	sawStop := make(chan bool, 1)
	orig := sweepExchangeDirs
	sweepExchangeDirs = func(_ string, _ time.Time, _ time.Duration, stopped func() bool) int {
		close(started)
		// The deadline is a failure guard, never the synchronization.
		deadline := time.Now().Add(time.Minute)
		for !stopped() {
			if time.Now().After(deadline) {
				sawStop <- false
				return 0
			}
			time.Sleep(time.Millisecond)
		}
		sawStop <- true
		return 0
	}
	t.Cleanup(func() { sweepExchangeDirs = orig })

	remote := startProviderSession(t, t.TempDir())
	<-started
	remote.Close()
	if !<-sawStop {
		t.Fatal("Close waited for the sweep without asking it to stop")
	}
}
