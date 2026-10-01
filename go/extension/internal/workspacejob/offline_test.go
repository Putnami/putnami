package workspacejob_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
)

const offlineSignal = "PUTNAMI_OFFLINE_DEPENDENCIES=1"

// setupEnv is the environment SetupGoEnv exports for environ, after setup.
func setupEnv(t *testing.T, cache string, setup func(*workspacejob.Job), environ ...string) []string {
	t.Helper()
	env := append([]string{"PATH=/usr/bin", "PUTNAMI_GO_CACHE_DIR=" + cache, "GOFLAGS=-trimpath"}, environ...)
	j := workspacejob.New(context.Background(), &jobtest.Recorder{}, env, t.TempDir(), "")
	setup(j)
	return j.Env.Environ()
}

// TestSetupGoEnvIsUnchangedWithoutTheOfflineSignal pins the environment of a
// lifecycle job the engine does not run offline: the one it exported before
// the offline signal existed, entry for entry and in order.
func TestSetupGoEnvIsUnchangedWithoutTheOfflineSignal(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"without-the-offline-signal-go-commands-are-unchanged")
	cache := t.TempDir()
	want := []string{
		"PATH=/usr/bin",
		"PUTNAMI_GO_CACHE_DIR=" + cache,
		"GOFLAGS=-trimpath",
		"GOCACHE=" + filepath.Join(cache, "build"),
		"GOMODCACHE=" + filepath.Join(cache, "mod"),
		"GOTOOLCHAIN=local",
		"GOPROXY=https://proxy.golang.org,direct",
		"GONOPROXY=vanity.example.test/*",
		"GONOSUMCHECK=vanity.example.test/*",
		"GONOSUMDB=vanity.example.test/*",
	}
	setup := func(j *workspacejob.Job) { j.SetupGoEnv("vanity.example.test") }
	if got := setupEnv(t, cache, setup); !reflect.DeepEqual(got, want) {
		t.Fatalf("SetupGoEnv without the offline signal =\n%q\nwant\n%q", got, want)
	}
	for _, value := range []string{"", "0", "true"} {
		signal := "PUTNAMI_OFFLINE_DEPENDENCIES=" + value
		wantWithSignal := append(append(append([]string(nil), want[:3]...), signal), want[3:]...)
		if got := setupEnv(t, cache, setup, signal); !reflect.DeepEqual(got, wantWithSignal) {
			t.Errorf("SetupGoEnv with %q =\n%q\nwant\n%q", signal, got, wantWithSignal)
		}
	}
}

// TestSetupGoEnvForbidsModuleDownloadsOffline pins the offline policy of the
// lifecycle jobs, which is toolchain.OfflineGoOverrides: SetupGoEnv applies it
// on the signal, and SetupFetchGoEnv, the environment of the job that does
// the downloading, never does.
func TestSetupGoEnvForbidsModuleDownloadsOffline(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"offline-go-commands-forbid-every-module-download")
	cache := t.TempDir()
	offline := workspacejob.NewEnv(setupEnv(t, cache, func(j *workspacejob.Job) {
		if !j.OfflineDependencies() {
			t.Error("OfflineDependencies() = false with the signal set")
		}
		j.SetupGoEnv("vanity.example.test")
	}, offlineSignal))
	for key, want := range map[string]string{
		"GOPROXY":     "off",
		"GONOPROXY":   "none",
		"GOFLAGS":     "-trimpath -mod=readonly",
		"GOTOOLCHAIN": "local",
		"GONOSUMDB":   "vanity.example.test/*",
	} {
		if got := offline.Get(key); got != want {
			t.Errorf("offline SetupGoEnv %s = %q, want %q", key, got, want)
		}
	}

	fetch := workspacejob.NewEnv(setupEnv(t, cache, func(j *workspacejob.Job) {
		j.SetupFetchGoEnv("vanity.example.test")
	}, offlineSignal))
	for key, want := range map[string]string{
		"GOPROXY":   "https://proxy.golang.org,direct",
		"GONOPROXY": "vanity.example.test/*",
		"GOFLAGS":   "-trimpath",
	} {
		if got := fetch.Get(key); got != want {
			t.Errorf("SetupFetchGoEnv %s = %q, want %q: the fetch job downloads", key, got, want)
		}
	}
}

// TestTrapRunsTheCleanupsAfterTheActionOnASignal pins OnSignal: a signal runs
// the armed action, then every cleanup still registered, the last registered
// first, then exits. A released cleanup does not run, and a replaced action
// does not replace a cleanup.
func TestTrapRunsTheCleanupsAfterTheActionOnASignal(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"a-signal-still-removes-the-fetch-credential")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	var order []string
	trap := workspacejob.NewTrap(cancel, func(code int) { order = append(order, "exit "+strconv.Itoa(code)) })
	releaseFirst := trap.OnSignal(func() { order = append(order, "first cleanup") })
	releaseGone := trap.OnSignal(func() { order = append(order, "released cleanup") })
	trap.OnSignal(func() { order = append(order, "last cleanup") })
	trap.Arm(func() { order = append(order, "restore") })
	releaseGone()
	releaseGone() // releasing twice changes nothing

	trap.Check()
	if len(order) != 0 {
		t.Fatalf("a trap without a signal acted: %v", order)
	}
	trap.Receive(syscall.SIGTERM)
	trap.Check()
	if want := []string{"restore", "last cleanup", "first cleanup", "exit 143"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("trap sequence = %v, want %v", order, want)
	}
	// The exit above ends the process in production. Here it returns, and the
	// cleanups it ran are spent.
	releaseFirst()
	trap.Disarm()
	if got := strings.Count(strings.Join(order, "\n"), "cleanup"); got != 2 {
		t.Fatalf("the cleanups ran %d times, want 2: %v", got, order)
	}

	var nilTrap *workspacejob.Trap
	nilTrap.OnSignal(func() {})()
}

// TestTrapOnSignalCatchesTheSignalWithoutAnAction proves OnSignal arms a trap
// no action armed: the signal reaches the trap, which runs the cleanup,
// instead of killing the process with the cleanup undone.
func TestTrapOnSignalCatchesTheSignalWithoutAnAction(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	var order []string
	trap := workspacejob.NewTrap(cancel, func(code int) { order = append(order, "exit "+strconv.Itoa(code)) })
	release := trap.OnSignal(func() { order = append(order, "cleanup") })
	defer release()
	trap.Deliver(syscall.SIGINT)
	trap.Disarm()
	if want := []string{"cleanup", "exit 130"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("trap sequence = %v, want %v", order, want)
	}
}
