package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The dependency-preparation stage as a DAG: what runs together, what stays
// exclusive, and what the stage says about where its time went.
//
// The stage sits on the critical path of EVERY run and used to walk its
// extensions one at a time. Making it concurrent is only worth doing if the
// three properties below survive it, so each has a test that fails for the
// right reason rather than by timing:
//
//   - independent chains really do overlap (a rendezvous the serial order
//     cannot satisfy, not a stopwatch);
//   - the content-addressed identity still governs reuse across a version
//     round trip, so A→B→A rebuilds nothing;
//   - the mutating step stays exclusive under real cross-PROCESS contention,
//     which is the case an in-process mutex would silently not cover.

// TestIndependentRuntimeSyncsRunConcurrently proves overlap by RENDEZVOUS
// rather than by measuring wall time.
//
// Every fixture's prepare command announces itself into a shared file and then
// refuses to finish until it can see the whole cohort there. Under the old
// serial loop the first prepare would wait for peers that cannot start until it
// returns, exhaust its budget and exit non-zero — so a regression to serial
// execution fails this test outright instead of making it slow. The cohort is
// sized to the fan-out the stage actually grants on this host, because that is
// the number the scheduler policy promises, not four.
func TestIndependentRuntimeSyncsRunConcurrently(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	const runtimes = 4
	workers := preparationWorkers(runtimes, runtime.NumCPU(), totalMemoryBytes())
	if workers < 2 {
		t.Skipf("host grants %d preparation worker(s); concurrency is not observable", workers)
	}

	barrier := filepath.Join(t.TempDir(), "cohort")
	artifacts := artifactstore.New(t.TempDir())

	exts := make([]*extension.ExtensionDescription, runtimes)
	for i := range exts {
		root := t.TempDir()
		name := fmt.Sprintf("@putnami/test-%d", i)
		writeRuntimeFixtureWithControls(t, root, name, "1.2.3", runtimeFixtureControls{
			barrierPath:  barrier,
			barrierCount: workers,
		})
		exts[i] = runtimeTestExtension(root)
		exts[i].Name = name
	}

	report := &PreparationReport{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := synchronizeExtensionRuntimes(ctx, exts, report, artifacts); err != nil {
		t.Fatalf("independent runtimes did not overlap: %v", err)
	}
	for i, ext := range exts {
		if err := validateRuntimeExecutable(ext.RuntimeExecutable); err != nil {
			t.Fatalf("runtime %d executable %q: %v", i, ext.RuntimeExecutable, err)
		}
	}
	if summary := report.Snapshot(); summary == nil || summary.Parallelism != workers {
		t.Fatalf("reported parallelism = %+v, want %d", summary, workers)
	}
}

// TestRuntimeSyncABAReuseRebuildsNothing is the content-addressing claim stated
// as an economic one: going A→B→A must cost ONE preparation for A and one for
// B, not three.
//
// It is the property the parallelization must not quietly break. A stage that
// keyed reuse on anything ambient — an in-process memo, the order the
// extensions were visited, a per-run directory — would still pass every
// correctness test here while paying for A twice, and the third leg is the only
// place that shows.
func TestRuntimeSyncABAReuseRebuildsNothing(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	counter := filepath.Join(t.TempDir(), "prepares")
	artifactDir := t.TempDir()
	artifacts := artifactstore.New(artifactDir)
	writeRuntimeFixtureWithControls(t, root, "@putnami/test", "1.2.3", runtimeFixtureControls{
		counterPath: counter,
	})
	source := filepath.Join(root, "cmd", "source")

	sync := func(t *testing.T, label string) (string, string) {
		t.Helper()
		ext := runtimeTestExtension(root)
		if err := synchronizeExtensionRuntimes(
			context.Background(), []*extension.ExtensionDescription{ext}, nil, artifacts,
		); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return ext.RuntimeDigest, ext.RuntimeExecutable
	}
	prepares := func(t *testing.T) int {
		t.Helper()
		data, err := os.ReadFile(counter)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "prepare\n")
	}

	digestA, executableA := sync(t, "A")
	if got := prepares(t); got != 1 {
		t.Fatalf("prepares after A = %d, want 1", got)
	}

	mustWriteRuntimeFile(t, source, "source-b", 0o644)
	digestB, executableB := sync(t, "B")
	if digestB == digestA {
		t.Fatal("version B reused version A's content identity")
	}
	if executableB == executableA {
		t.Fatalf("version B resolved to A's executable %q", executableA)
	}
	if got := prepares(t); got != 2 {
		t.Fatalf("prepares after A→B = %d, want 2", got)
	}

	mustWriteRuntimeFile(t, source, "source", 0o644)
	digestAgain, executableAgain := sync(t, "A again")
	if digestAgain != digestA || executableAgain != executableA {
		t.Fatalf("A→B→A resolved %q/%q, want the original %q/%q",
			digestAgain, executableAgain, digestA, executableA)
	}
	if got := prepares(t); got != 2 {
		t.Fatalf("prepares after A→B→A = %d, want 2 — the return to A must be pure reuse", got)
	}
	// Both identities remain published, which is what makes the round trip free
	// in either direction rather than only on the way back.
	for label, digest := range map[string]string{"A": digestA, "B": digestB} {
		if !artifacts.Has(digest) {
			t.Errorf("identity %s (%s) is not published", label, digest)
		}
	}
}

// TestRuntimeSyncCancellationLeavesNoTornState covers the cancellation half of
// the slice: a run interrupted while a prepare is in flight must leave the
// shared store exactly as it found it, and the next run must be able to finish
// the job.
//
// The two assertions are deliberately about the STORE rather than about the
// error. A half-built runtime published under its final digest would be
// indistinguishable from a good one forever afterwards — content addressing
// makes a poisoned entry permanent — so "nothing was published" and "no staging
// tree was left behind" are the properties that matter, and the recovery run
// proves the interruption cost nothing but its own wall.
func TestRuntimeSyncCancellationLeavesNoTornState(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	artifactDir := t.TempDir()
	artifacts := artifactstore.New(artifactDir)
	releasePath := filepath.Join(t.TempDir(), "release")
	writeRuntimeFixtureWithControls(t, root, "@putnami/test", "1.2.3", runtimeFixtureControls{
		releasePath: releasePath,
	})
	ext := runtimeTestExtension(root)

	digest, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	interrupted := make(chan error, 1)
	go func() {
		interrupted <- synchronizeExtensionRuntimes(
			ctx, []*extension.ExtensionDescription{ext}, nil, artifacts,
		)
	}()
	// Cancel once the prepare subprocess is demonstrably running, so the
	// interruption lands mid-preparation rather than before it starts.
	waitForPrepareToStart(t, artifactDir)
	cancel()

	var runtimeErr *extensionRuntimeError
	select {
	case err := <-interrupted:
		if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimePrepareFailed {
			t.Fatalf("interruption error = %v, want %s", err, extensionproto.FailureRuntimePrepareFailed)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("cancellation did not unwind the stage")
	}
	if ext.RuntimeExecutable != "" {
		t.Fatalf("interrupted sync published executable %q", ext.RuntimeExecutable)
	}

	if artifacts.Has(digest) {
		t.Fatalf("interrupted preparation published digest %s", digest)
	}
	assertNoLeftoverStaging(t, artifactDir)

	mustWriteRuntimeFile(t, releasePath, "release\n", 0o644)
	recovered := runtimeTestExtension(root)
	if err := synchronizeExtensionRuntimes(
		context.Background(), []*extension.ExtensionDescription{recovered}, nil, artifacts,
	); err != nil {
		t.Fatalf("recovery after cancellation: %v", err)
	}
	if recovered.RuntimeDigest != digest || !artifacts.Has(digest) {
		t.Fatalf("recovery published %q, want the same identity %q", recovered.RuntimeDigest, digest)
	}
	assertNoLeftoverStaging(t, artifactDir)
}

// TestRuntimeSyncIsExclusiveAcrossProcesses is the concurrency case an
// in-process mutex cannot cover: several putnami PROCESSES synchronizing the
// same workspace at once.
//
// It runs the real stage in real child processes against one machine-global
// store, and asserts what exclusivity means here — the expensive step happens
// ONCE, every process is handed the same published tree, and no staging
// directory survives. flock(2) keys on the open file description, so the same
// lock that holds sibling goroutines apart is the one being exercised here;
// this test is what proves the claim rather than assuming it.
func TestRuntimeSyncIsExclusiveAcrossProcesses(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("flock ownership is Unix-only")
	}
	root := t.TempDir()
	artifactDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "prepares")
	writeRuntimeFixture(t, root, "@putnami/test", "1.2.3")
	const processes = 4

	cmds := make([]*exec.Cmd, processes)
	outputs := make([]bytes.Buffer, processes)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestRuntimeSyncHelperProcess$", "-test.timeout=120s")
		cmds[i].Stdout = &outputs[i]
		cmds[i].Stderr = &outputs[i]
		cmds[i].Env = append(os.Environ(),
			"PUTNAMI_RUNTIME_SYNC_HELPER=1",
			"PUTNAMI_ARTIFACT_DIR="+artifactDir,
			"RUNTIME_PREPARE_COUNTER="+counter,
			// A slow prepare guarantees the losers arrive while the winner still
			// holds the digest lock, which is the contention this test is for.
			"RUNTIME_PREPARE_SLEEP=1",
			"RUNTIME_SYNC_HELPER_ROOT="+root,
			"RUNTIME_SYNC_HELPER_WORKSPACE="+t.TempDir(),
		)
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	resolved := make([]string, processes)
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v: %s", i, err, outputs[i].String())
		}
		resolved[i] = helperResolvedPath(t, outputs[i].String())
		if resolved[i] != resolved[0] {
			t.Fatalf("helper %d resolved %q, want %q", i, resolved[i], resolved[0])
		}
	}
	if err := validateRuntimeExecutable(resolved[0]); err != nil {
		t.Fatalf("published runtime %q after concurrent processes: %v", resolved[0], err)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "prepare\n"); got != 1 {
		t.Fatalf("cross-process prepares = %d, want exactly 1", got)
	}
	assertNoLeftoverStaging(t, artifactDir)
}

// TestRuntimeSyncHelperProcess is one putnami process for the test above. It is
// inert unless the parent asks for it.
func TestRuntimeSyncHelperProcess(t *testing.T) {
	if os.Getenv("PUTNAMI_RUNTIME_SYNC_HELPER") != "1" {
		return
	}
	ext := runtimeTestExtension(os.Getenv("RUNTIME_SYNC_HELPER_ROOT"))
	ws := &workspace.Workspace{Root: os.Getenv("RUNTIME_SYNC_HELPER_WORKSPACE")}
	if err := SynchronizeExtensionRuntimes(
		context.Background(), ws, []*extension.ExtensionDescription{ext}, nil,
	); err != nil {
		t.Fatalf("helper synchronize: %v", err)
	}
	fmt.Printf("%s%s\n", helperResolvedPrefix, ext.RuntimeExecutable)
}

const helperResolvedPrefix = "RUNTIME_SYNC_RESOLVED "

func helperResolvedPath(t *testing.T, output string) string {
	t.Helper()
	for line := range strings.SplitSeq(output, "\n") {
		if path, found := strings.CutPrefix(strings.TrimSpace(line), helperResolvedPrefix); found {
			return path
		}
	}
	t.Fatalf("helper printed no resolved runtime:\n%s", output)
	return ""
}

// TestPreparationAttributionDecomposesTheStage pins the attribution against the
// two states that matter: a cold stage that had to build, and a warm one that
// did not.
//
// The warm case carries the load-bearing assertion. GENERATION MUST BE ABSENT,
// not zero — the epic's whole question is which of the five ownership classes
// is actually paying, and a phase reported with 0 would say "we ran the compile
// and it was free", which is the opposite of what reuse means.
func TestPreparationAttributionDecomposesTheStage(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	artifacts := artifactstore.New(t.TempDir())
	// The generation metric is the prepare shell's own ProcessState CPU. Burn a
	// small, bounded amount of builtin work in that exact process: mkdir, cp and
	// chmod otherwise spend their CPU in grandchildren and can legitimately
	// leave the shell below the kernel's accounting granularity.
	writeRuntimeFixtureWithControls(t, root, "@putnami/test", "1.2.3", runtimeFixtureControls{
		cpuWorkIterations: 20_000,
	})

	cold := &PreparationReport{}
	if err := synchronizeExtensionRuntimes(
		context.Background(), []*extension.ExtensionDescription{runtimeTestExtension(root)}, cold, artifacts,
	); err != nil {
		t.Fatal(err)
	}
	coldPhases := phaseIndex(t, cold)
	for _, phase := range []PreparationPhase{
		PreparationResolution, PreparationVerification, PreparationGeneration, PreparationMutation,
	} {
		entry, entered := coldPhases[phase]
		if !entered {
			t.Fatalf("cold stage did not enter %s: %+v", phase, coldPhases)
		}
		if entry.Steps < 1 {
			t.Errorf("%s recorded %d steps, want at least 1", phase, entry.Steps)
		}
	}
	// Generation and verification spawn subprocesses, so they carry MEASURED
	// child CPU; the in-process classes carry none rather than a derived figure.
	if coldPhases[PreparationGeneration].CPU <= 0 {
		t.Error("generation reported no child CPU for a subprocess it waited on")
	}
	if coldPhases[PreparationResolution].CPU != 0 || coldPhases[PreparationMutation].CPU != 0 {
		t.Errorf("in-process phases invented child CPU: %+v", coldPhases)
	}
	// The stage never talks to a remote: installed binaries are materialized by
	// the lock-driven ensure pass, and a prepare command's own downloads happen
	// inside the subprocess counted under generation.
	if _, entered := coldPhases[PreparationNetwork]; entered {
		t.Error("runtime synchronization claimed network time it does not spend")
	}

	warm := &PreparationReport{}
	if err := synchronizeExtensionRuntimes(
		context.Background(), []*extension.ExtensionDescription{runtimeTestExtension(root)}, warm, artifacts,
	); err != nil {
		t.Fatal(err)
	}
	warmPhases := phaseIndex(t, warm)
	if entry, entered := warmPhases[PreparationGeneration]; entered {
		t.Errorf("warm stage reported generation %+v; reuse builds nothing and says nothing", entry)
	}
	for _, phase := range []PreparationPhase{
		PreparationResolution, PreparationVerification, PreparationMutation,
	} {
		if _, entered := warmPhases[phase]; !entered {
			t.Errorf("warm stage did not enter %s: %+v", phase, warmPhases)
		}
	}
}

// TestPreparationPhasesFitTheStageWallWhenSerial pins the producer side of the
// protocol's arithmetic rule.
//
// One node means one worker means no overlap, so the phase spans must be a
// partition of the stage's own wall. Above one worker they may overlap, and the
// contract says so — but only because the serial case is exact here. If this
// test failed, the reported decomposition would be double-counting somewhere
// and the parallel numbers would be uninterpretable too.
func TestPreparationPhasesFitTheStageWallWhenSerial(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	artifacts := artifactstore.New(t.TempDir())
	writeRuntimeFixture(t, root, "@putnami/test", "1.2.3")

	report := &PreparationReport{}
	if err := synchronizeExtensionRuntimes(
		context.Background(), []*extension.ExtensionDescription{runtimeTestExtension(root)}, report, artifacts,
	); err != nil {
		t.Fatal(err)
	}
	summary := report.Snapshot()
	if summary == nil {
		t.Fatal("a stage that prepared a runtime reported nothing")
	}
	if summary.Parallelism != 1 {
		t.Fatalf("single node ran at parallelism %d, want 1", summary.Parallelism)
	}
	var phased time.Duration
	for _, phase := range summary.Phases {
		phased += phase.Wall
	}
	if phased > summary.Wall {
		t.Fatalf("serial phases sum to %s inside a %s stage: %+v", phased, summary.Wall, summary.Phases)
	}
}

// TestPreparationWorkersPolicy pins the fan-out policy against the reasons for
// it: every node is a compile that already fans out inside its own toolchain,
// so the bound is the visible core count rather than the scheduler's
// oversubscription — and the heavy-plan memory cap applies, because that is
// exactly what a plan of compiles is.
func TestPreparationWorkersPolicy(t *testing.T) {
	const gib = uint64(1024 * 1024 * 1024)
	for _, tc := range []struct {
		name   string
		nodes  int
		cpus   int
		memory uint64
		want   int
	}{
		{name: "single node never forks a worker pool", nodes: 1, cpus: 16, memory: 64 * gib, want: 1},
		{name: "bounded by the node count", nodes: 3, cpus: 16, memory: 64 * gib, want: 3},
		{name: "bounded by visible cores, not a multiple of them", nodes: 12, cpus: 4, memory: 64 * gib, want: 4},
		{name: "bounded by memory on a small machine", nodes: 8, cpus: 8, memory: 3 * gib, want: 2},
		{name: "unknown memory falls back to the cpu bound", nodes: 8, cpus: 2, memory: 0, want: 2},
		{name: "never below one", nodes: 4, cpus: 0, memory: 0, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preparationWorkers(tc.nodes, tc.cpus, tc.memory); got != tc.want {
				t.Errorf("preparationWorkers(%d, %d, %d) = %d, want %d",
					tc.nodes, tc.cpus, tc.memory, got, tc.want)
			}
		})
	}
}

// TestRuntimeSyncPlanDeduplicatesAndFailsClosed covers the plan pass, which is
// where concurrency safety starts.
//
// De-duplication is BY POINTER because the hazard is two workers writing one
// description's RuntimeExecutable, not two descriptions naming one extension.
// And an undeclared runtime is reported before any preparation begins: with
// concurrent nodes there is no "first failure wins by position" any more, so
// the manifest error — the one with a fixed remedy — is hoisted ahead of the
// build errors rather than racing them.
func TestRuntimeSyncPlanDeduplicatesAndFailsClosed(t *testing.T) {
	ext := runtimeTestExtension(t.TempDir())
	nodes, err := planExtensionRuntimeSync([]*extension.ExtensionDescription{ext, nil, ext, ext})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0] != ext {
		t.Fatalf("plan = %d node(s), want the one description exactly once", len(nodes))
	}

	undeclared := &extension.ExtensionDescription{
		Name: "@putnami/no-runtime",
		Jobs: map[string]*extension.JobDefinition{
			"build": {Name: "build", CommandName: "build", Command: "{extensionRuntime}"},
		},
	}
	for _, order := range [][]*extension.ExtensionDescription{
		{undeclared, ext},
		{ext, undeclared},
	} {
		nodes, err := planExtensionRuntimeSync(order)
		var runtimeErr *extensionRuntimeError
		if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeNotDeclared {
			t.Fatalf("plan error = %v, want %s regardless of position",
				err, extensionproto.FailureRuntimeNotDeclared)
		}
		if nodes != nil {
			t.Fatalf("a rejected plan still offered %d node(s) to prepare", len(nodes))
		}
	}
}

// waitForPrepareToStart blocks until a staging tree exists under the artifact
// store, which is the observable signal that the prepare subprocess is running
// against it.
func waitForPrepareToStart(t *testing.T, artifactDir string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(filepath.Join(artifactDir, "tmp"))
		if err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "staging-") {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no staging tree appeared; the prepare never started")
}

// assertNoLeftoverStaging fails when a staging tree outlived the admit that
// created it — the signature of an interrupted stage that left the shared store
// dirty for every later run and every other worktree on the machine.
func assertNoLeftoverStaging(t *testing.T, artifactDir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(artifactDir, "tmp"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "staging-") {
			t.Errorf("leftover staging tree: %s", entry.Name())
		}
	}
}

func phaseIndex(t *testing.T, report *PreparationReport) map[PreparationPhase]PreparationPhaseTotals {
	t.Helper()
	summary := report.Snapshot()
	if summary == nil {
		t.Fatal("stage reported no attribution")
	}
	if summary.Wall <= 0 {
		t.Errorf("stage wall = %s, want a positive measurement", summary.Wall)
	}
	index := make(map[PreparationPhase]PreparationPhaseTotals, len(summary.Phases))
	for _, phase := range summary.Phases {
		index[phase.Phase] = phase
	}
	return index
}
