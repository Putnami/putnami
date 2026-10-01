package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// cacheCommandExtension builds an extension whose reserved cache commands run
// program, so a test can observe exactly what core hands them.
func cacheCommandExtension(t *testing.T, name string, program fixtureproc.Program) *extension.ExtensionDescription {
	t.Helper()
	root := t.TempDir()
	path := fixtureproc.Write(t, filepath.Join(root, "cache-command"), program)
	jobs := make(map[string]*extension.JobDefinition)
	for _, command := range extensionproto.ReservedCacheCommands {
		jobs[command] = &extension.JobDefinition{
			Name:        command,
			CommandName: command,
			Kind:        "command",
			Command:     path,
			Args:        []string{command},
		}
	}
	return &extension.ExtensionDescription{Name: name, Path: root, Jobs: jobs}
}

func TestExtensionsDeclaringCacheCommand(t *testing.T) {
	declaring := cacheCommandExtension(t, "@test/b", fixtureproc.Program{})
	first := cacheCommandExtension(t, "@test/a", fixtureproc.Program{})
	silent := &extension.ExtensionDescription{Name: "@test/silent"}

	got := ExtensionsDeclaringCacheCommand(
		[]*extension.ExtensionDescription{declaring, silent, nil, first},
		extensionproto.CommandCacheClean,
	)
	if len(got) != 2 {
		t.Fatalf("selected %d extensions, want 2", len(got))
	}
	// Name order is what makes the fan-out's output deterministic.
	if got[0].Name != "@test/a" || got[1].Name != "@test/b" {
		t.Fatalf("selection order = %q, %q — want name order", got[0].Name, got[1].Name)
	}
	if ExtensionsDeclaringCacheCommand([]*extension.ExtensionDescription{declaring}, "build") != nil {
		t.Error("a non-reserved command name selected extensions")
	}
}

// TestRunCacheCommands_AsksEveryExtension is the aggregation contract at the
// runner level: a failing extension contributes an error and NEVER a byte count,
// and the extensions after it are still asked.
func TestRunCacheCommands_AsksEveryExtension(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir()}
	t.Setenv(extensionproto.MachineCacheDirEnv, filepath.Join(ws.Root, "machine-caches"))

	broken := cacheCommandExtension(t, "@test/broken", fixtureproc.Program{Stderr: "boom\n", Exit: 3})
	// A failing extension that ALSO printed a summary must still not be counted
	// as having freed anything.
	lying := cacheCommandExtension(t, "@test/lying", fixtureproc.Program{
		Stdout: `{"v":1,"type":"summary","data":{"freedBytes":9999}}` + "\n",
		Exit:   1,
	})
	healthy := cacheCommandExtension(t, "@test/healthy", fixtureproc.Program{
		Stdout: `{"v":1,"type":"summary","data":{"freedBytes":2048}}` + "\n",
	})

	outcomes := RunCacheCommands(
		context.Background(), ws,
		[]*extension.ExtensionDescription{broken, lying, healthy},
		extensionproto.CommandCacheClean, false,
	)
	if len(outcomes) != 3 {
		t.Fatalf("got %d outcomes, want one per extension", len(outcomes))
	}
	if outcomes[0].Err == nil || outcomes[1].Err == nil {
		t.Fatalf("failing extensions reported no error: %+v", outcomes)
	}
	if outcomes[1].FreedBytes != 0 {
		t.Errorf("a failed collection reported %d bytes freed", outcomes[1].FreedBytes)
	}
	if !strings.Contains(outcomes[0].Err.Error(), "boom") {
		t.Errorf("error does not carry the extension's stderr: %v", outcomes[0].Err)
	}
	if outcomes[2].Err != nil || outcomes[2].FreedBytes != 2048 {
		t.Errorf("healthy extension outcome = %+v — the fan-out aborted early", outcomes[2])
	}
}

// TestRunCacheCommand_DeliversTheContractRoots pins the three roots a cache
// command receives and that core creates the machine one.
func TestRunCacheCommand_DeliversTheContractRoots(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir()}
	machineParent := filepath.Join(ws.Root, "machine-caches")
	t.Setenv(extensionproto.MachineCacheDirEnv, machineParent)

	record := filepath.Join(t.TempDir(), "runs.jsonl")
	ext := cacheCommandExtension(t, "@test/roots", fixtureproc.Program{
		Record: record,
		Stdout: `{"v":1,"type":"summary","data":{"freedBytes":0}}` + "\n",
	})

	if _, err := RunCacheCommand(context.Background(), ws, ext, extensionproto.CommandCacheGC, false); err != nil {
		t.Fatalf("RunCacheCommand: %v", err)
	}

	run := onlyRun(t, record)
	read := func(name string) string {
		t.Helper()
		value, ok := run.LookupEnv(name)
		if !ok {
			t.Fatalf("the cache command ran without %s", name)
		}
		return value
	}
	wantMachine := filepath.Join(machineParent, "@test-roots")
	if got := read("PUTNAMI_EXTENSION_CACHE_ROOT"); got != wantMachine {
		t.Errorf("PUTNAMI_EXTENSION_CACHE_ROOT = %q, want %q", got, wantMachine)
	}
	if _, err := os.Stat(wantMachine); err != nil {
		t.Errorf("core did not create the extension's machine cache root: %v", err)
	}
	if got, want := read("PUTNAMI_CACHE_ROOT"), filepath.Join(ws.Root, ".putnami", "cache"); got != want {
		t.Errorf("PUTNAMI_CACHE_ROOT = %q, want %q", got, want)
	}
	if got := read("PUTNAMI_CACHE_COMMAND"); got != extensionproto.CommandCacheGC {
		t.Errorf("PUTNAMI_CACHE_COMMAND = %q, want %q", got, extensionproto.CommandCacheGC)
	}
	if got := read("PUTNAMI_EXTENSION_NAME"); got != "@test/roots" {
		t.Errorf("PUTNAMI_EXTENSION_NAME = %q, want @test/roots", got)
	}
}

func TestRunCacheCommand_RejectsUnknownCommand(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir()}
	ext := cacheCommandExtension(t, "@test/x", fixtureproc.Program{})
	if _, err := RunCacheCommand(context.Background(), ws, ext, "cache-purge", false); err == nil {
		t.Fatal("an unreserved cache command was accepted")
	}
	// An extension that declares nothing is a no-op, not an error.
	silent := &extension.ExtensionDescription{Name: "@test/silent"}
	freed, err := RunCacheCommand(context.Background(), ws, silent, extensionproto.CommandCacheGC, false)
	if err != nil || freed != 0 {
		t.Fatalf("undeclared cache command = (%d, %v), want (0, nil)", freed, err)
	}
}

// TestOpportunisticCacheGCDue reserves the window on the way IN. Reserving after
// the collection instead would let a crashed collector re-trigger on every run,
// and would let two concurrent putnami processes both decide it is due.
func TestOpportunisticCacheGCDue(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir()}
	t.Setenv(extensionproto.MachineCacheDirEnv, filepath.Join(ws.Root, "machine-caches"))
	ext := cacheCommandExtension(t, "@test/throttled", fixtureproc.Program{})

	if !OpportunisticCacheGCDue(ws, ext, time.Hour) {
		t.Fatal("first call must be due")
	}
	if OpportunisticCacheGCDue(ws, ext, time.Hour) {
		t.Fatal("second call inside the window must not be due")
	}
	stamp := filepath.Join(extensionproto.MachineCacheRoot(ext.Name, ws.Root), OpportunisticGCStampFile)
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("throttle stamp was not written: %v", err)
	}
	// A zero/negative interval is "never opportunistically collect".
	if OpportunisticCacheGCDue(ws, ext, 0) {
		t.Error("a zero interval must not be due")
	}
	if OpportunisticCacheGCDue(nil, ext, time.Hour) || OpportunisticCacheGCDue(ws, nil, time.Hour) {
		t.Error("a nil workspace or extension must not be due")
	}
}

// TestOpportunisticCacheGCDue_ConcurrentReservations verifies that worktrees
// sharing one machine cache root cannot each reserve the same collection
// window. The lock is intentionally exercised by goroutines here: flock takes
// effect across separate descriptors even when they belong to one process.
func TestOpportunisticCacheGCDue_ConcurrentReservations(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir()}
	t.Setenv(extensionproto.MachineCacheDirEnv, filepath.Join(ws.Root, "machine-caches"))
	ext := cacheCommandExtension(t, "@test/concurrent-throttle", fixtureproc.Program{})

	const callers = 32
	start := make(chan struct{})
	results := make(chan bool, callers)
	for range callers {
		go func() {
			<-start
			results <- OpportunisticCacheGCDue(ws, ext, time.Hour)
		}()
	}
	close(start)

	due := 0
	for range callers {
		if <-results {
			due++
		}
	}
	if due != 1 {
		t.Fatalf("concurrent due callers = %d, want 1", due)
	}
}

// TestMaybeOpportunisticCacheGC_SkipsUnresolvedRuntimeWithoutBurningTheWindow:
// an extension whose runtime this run never prepared cannot collect, and
// reserving its hour anyway would leave its cache uncollected every time an
// unrelated run finished.
func TestMaybeOpportunisticCacheGC_SkipsUnresolvedRuntimeWithoutBurningTheWindow(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir()}
	t.Setenv(extensionproto.MachineCacheDirEnv, filepath.Join(ws.Root, "machine-caches"))

	unprepared := &extension.ExtensionDescription{
		Name: "@test/unprepared",
		Path: t.TempDir(),
		Jobs: map[string]*extension.JobDefinition{
			extensionproto.CommandCacheGC: {
				Name:    extensionproto.CommandCacheGC,
				Command: "{extensionRuntime}",
				Args:    []string{extensionproto.CommandCacheGC},
			},
		},
	}

	started := MaybeOpportunisticCacheGC(ws, []*extension.ExtensionDescription{unprepared}, time.Hour)
	if len(started) != 0 {
		t.Fatalf("started %v for an extension with no prepared runtime", started)
	}
	stamp := filepath.Join(extensionproto.MachineCacheRoot(unprepared.Name, ws.Root), OpportunisticGCStampFile)
	if _, err := os.Stat(stamp); err == nil {
		t.Fatal("the throttle window was reserved for an extension that could not collect")
	}
}

// TestMaybeOpportunisticCacheGC_StartsOncePerInterval is the throttle in its
// real shape: a runnable extension starts once, then not again inside the window.
func TestMaybeOpportunisticCacheGC_StartsOncePerInterval(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir()}
	t.Setenv(extensionproto.MachineCacheDirEnv, filepath.Join(ws.Root, "machine-caches"))
	marker := filepath.Join(ws.Root, "collected")
	ext := cacheCommandExtension(t, "@test/collector", fixtureproc.Program{Record: marker})

	started := MaybeOpportunisticCacheGC(ws, []*extension.ExtensionDescription{ext}, time.Hour)
	if len(started) != 1 || started[0] != "@test/collector" {
		t.Fatalf("started = %v, want the one runnable extension", started)
	}
	if again := MaybeOpportunisticCacheGC(ws, []*extension.ExtensionDescription{ext}, time.Hour); len(again) != 0 {
		t.Fatalf("started = %v inside the throttle window", again)
	}

	// The detached child races this assertion by design; wait for its marker
	// rather than assuming it has been scheduled. The deadline is a backstop
	// against a collector that never starts, not a latency budget: under a
	// fully loaded gate (this package shares cores with race+coverage suites)
	// 5s passed before the detached shell was even scheduled.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the detached collector never ran")
}
