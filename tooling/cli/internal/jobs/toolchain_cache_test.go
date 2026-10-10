package jobs

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	model "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// The ambient-tool version probe is tested against a REAL child process, not a
// stub.
//
// Everything this probe is about is a process-boundary property — a binary that never
// returns, a binary that writes without end — and none of it is observable
// through the in-process fake the earlier tests here installed. The child is this
// test binary re-executed through a per-test hard link whose basename carries
// the mode (see TestMain in remote_provider_test.go). Unlike an environment
// selector, the path is local to one probe and remains safe under t.Parallel;
// the child still needs no shell or fixture binary.
const toolProbeHelperPrefix = "putnami-jobs-tool-probe-"

// toolProbeHelperVersion is what the "version" child prints. It stands in for a
// real toolchain's single-line output.
const toolProbeHelperVersion = "1.2.3"

// runToolProbeHelper is the child. It is called from TestMain before the testing
// harness parses flags, because the probe invokes it as `<binary> --version`.
func runToolProbeHelper(mode string) {
	switch mode {
	case "version":
		os.Stdout.WriteString(toolProbeHelperVersion + "\n") //nolint:errcheck // child process output
	case "silent":
	case "chatty":
		os.Stdout.WriteString("warning: config is deprecated\n1.2.3\n") //nolint:errcheck // child process output
	case "reserved":
		os.Stdout.WriteString(toolVersionTimeout + "\n") //nolint:errcheck // child process output
	case "fail":
		os.Stderr.WriteString("not a toolchain\n") //nolint:errcheck // child process diagnostics
		os.Exit(3)
	case "flood":
		// Far past the cap, then a clean exit: this is the "writes without end"
		// case minus the waiting, so the assertion is about the CAP and not about
		// the deadline that would otherwise mask it.
		chunk := bytes.Repeat([]byte("a"), 64*1024)
		for range 16 {
			os.Stdout.Write(chunk) //nolint:errcheck // child process output
		}
	case "hang":
		// Sleep rather than block on a channel: a blocked main goroutine trips
		// the runtime's deadlock detector, which would end the child on its own
		// and let a probe with no deadline pass this test. It wakes every 10 ms,
		// so a host that samples its progress sees it run whenever it samples.
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end); {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// helperTool points the probe at this test binary in the given mode. A hard
// link is cheap even for a race-instrumented test binary and preserves argv[0],
// which is the only selector TestMain needs. Windows refuses to delete any name
// of a running image, so a link to this test binary would outlive the TempDir
// cleanup there; Windows gets an independent copy instead.
func helperTool(t *testing.T, mode string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := toolProbeHelperPrefix + mode
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		if err := copyRuntimeInputFile(executable, path, 0o755); err != nil {
			t.Fatalf("copy tool probe helper: %v", err)
		}
		return path
	}
	if err := os.Link(executable, path); err != nil {
		t.Fatalf("link tool probe helper: %v", err)
	}
	return path
}

func toolProbeHelperMode(executable string) (string, bool) {
	name := filepath.Base(executable)
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	return strings.CutPrefix(name, toolProbeHelperPrefix)
}

// TestProbeToolVersionHappyPathValueIsUnchanged guards the cache-key regression
// adding the bound could have caused: a healthy toolchain's identity must
// stay byte-identical to the pre-fix `strings.TrimSpace(CombinedOutput())`, or
// every TypeScript cache entry on every machine moves at once.
func TestProbeToolVersionHappyPathValueIsUnchanged(t *testing.T) {
	t.Parallel()
	path := helperTool(t, "version")
	if got := probeToolVersion(path, hostProcessDeadline(30*time.Second)); got != toolProbeHelperVersion {
		t.Fatalf("probe = %q, want the trimmed version line %q", got, toolProbeHelperVersion)
	}

	// ...and the whole resolution, warning included: a warning on every healthy
	// run would train the reader to ignore the one that matters.
	logs, logger := captureWarnings()

	resolved := resolveAmbientToolVersionWith(
		"bun",
		func(string) (string, error) { return path, nil },
		logger,
	)
	if resolved != toolProbeHelperVersion {
		t.Fatalf("resolveAmbientToolVersion = %q, want %q", resolved, toolProbeHelperVersion)
	}
	if logs.Len() != 0 {
		t.Fatalf("a healthy probe warned: %q", logs.String())
	}
}

// A tool that never returns used to hang the first cache-key computation, and
// with it every other one waiting on the same sync.OnceValue: no task could be
// scheduled.
func TestProbeToolVersionBoundsAChildThatNeverReturns(t *testing.T) {
	t.Parallel()
	path := helperTool(t, "hang")

	start := time.Now()
	got := probeToolVersion(path, hostProcessDeadline(time.Second))
	elapsed := time.Since(start)

	if got != toolVersionTimeout {
		t.Fatalf("probe = %q, want %q", got, toolVersionTimeout)
	}
	// The child sleeps for 30s. Returning at all before then means the deadline —
	// and not the child — ended the probe.
	if elapsed > 20*time.Second {
		t.Fatalf("probe took %v; the deadline did not bound the child", elapsed)
	}
	// A timeout must not read as "not installed": that host HAS the tool on PATH
	// and can still run tasks with it.
	if got == toolVersionUnavailable {
		t.Fatal("a wedged toolchain collapsed onto the missing-tool identity")
	}
}

// A binary that writes far past any version line must cost a fixed number of
// bytes and must not have its first 4KB of noise adopted as a cache identity.
func TestProbeToolVersionRejectsOversizedOutput(t *testing.T) {
	t.Parallel()
	got := probeToolVersion(helperTool(t, "flood"), hostProcessDeadline(30*time.Second))
	if got != toolVersionInvalid {
		t.Fatalf("probe = %q, want %q", got, toolVersionInvalid)
	}
	if strings.Contains(got, "aaa") {
		t.Fatalf("probe adopted the child's output as an identity: %q", got)
	}
}

func TestProbeToolVersionDegradedIdentities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mode string
		want string
	}{
		// Identities from before this bound existed. These must not move: an
		// environment with no usable toolchain keys exactly where it always did.
		{name: "non-zero exit", mode: "fail", want: toolVersionUnavailable},
		{name: "empty output", mode: "silent", want: toolVersionUnknown},
		// A version line is one short line. Anything else is an executable that
		// is not answering the question.
		{name: "multi-line output", mode: "chatty", want: toolVersionInvalid},
		// A binary that prints a reserved identity must not be able to claim a
		// degraded one as a successful one.
		{name: "reserved identity as version", mode: "reserved", want: toolVersionInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := probeToolVersion(helperTool(t, tc.mode), hostProcessDeadline(30*time.Second)); got != tc.want {
				t.Fatalf("probe = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProbeToolVersionDeadlineCountsFromTheFirstInstruction pins that a
// probe the host holds before its first instruction keeps its identity: held
// past a deadline that has already elapsed, it still answers its version. A
// hold past the admission bound is a timeout, never a missing tool. No case
// depends on how fast the host runs.
func TestProbeToolVersionDeadlineCountsFromTheFirstInstruction(t *testing.T) {
	t.Parallel()
	held := processDeadline{run: 0, admission: elapsesNever, progress: neverRunning}
	if got := probeToolVersion(helperTool(t, "version"), held); got != toolProbeHelperVersion {
		t.Fatalf("probe held past its deadline = %q, want %q", got, toolProbeHelperVersion)
	}
	heldPastBound := processDeadline{run: elapsesNever, admission: 0, progress: neverRunning}
	if got := probeToolVersion(helperTool(t, "hang"), heldPastBound); got != toolVersionTimeout {
		t.Fatalf("probe held past the admission bound = %q, want %q", got, toolVersionTimeout)
	}
}

func TestProbeToolVersionMissingExecutableIsUnavailable(t *testing.T) {
	t.Parallel()
	got := probeToolVersion(filepath.Join(t.TempDir(), "tool"), hostProcessDeadline(30*time.Second))
	if got != toolVersionUnavailable {
		t.Fatalf("probe = %q, want %q", got, toolVersionUnavailable)
	}
}

// Every degraded identity must be distinct from every other one and from every
// plausible successful one — that is what keeps a probe that never learned the
// compiler's version from sharing a key with one that did.
func TestDegradedToolIdentitiesAreDistinct(t *testing.T) {
	t.Parallel()
	identities := []string{toolVersionUnavailable, toolVersionUnknown, toolVersionTimeout, toolVersionInvalid}
	seen := map[string]bool{}
	for _, identity := range identities {
		if seen[identity] {
			t.Fatalf("degraded identity %q is used for more than one failure mode", identity)
		}
		seen[identity] = true
		if !isDegradedToolVersion(identity) {
			t.Fatalf("%q is not recognized as degraded", identity)
		}
	}
	if isDegradedToolVersion("1.2.3") {
		t.Fatal("a real version was classified as degraded")
	}
}

// The resolution is generic — it identifies whatever ambient tool it is named,
// and only the one caller in cacheToolchainVersion knows which one that is.
func TestResolveAmbientToolVersionReportsAMissingToolWithoutProbing(t *testing.T) {
	t.Parallel()
	var looked string
	lookup := func(tool string) (string, error) {
		looked = tool
		return "", errors.New("not found")
	}

	if got := resolveAmbientToolVersionWith("acme-compiler", lookup, slog.Default()); got != toolVersionUnavailable {
		t.Fatalf("resolveAmbientToolVersion = %q, want %q", got, toolVersionUnavailable)
	}
	if looked != "acme-compiler" {
		t.Fatalf("looked up %q, want the tool it was asked about", looked)
	}
}

// A degraded cache identity is silent in the key alone — the run still succeeds,
// just against artifacts that are not pinned to a tool version. This test asks
// for it to be visible.
func TestResolveAmbientToolVersionWarnsOnADegradedIdentity(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "tool")
	logs, logger := captureWarnings()

	if got := resolveAmbientToolVersionWith(
		"bun",
		func(string) (string, error) { return missing, nil },
		logger,
	); got != toolVersionUnavailable {
		t.Fatalf("resolveAmbientToolVersion = %q, want %q", got, toolVersionUnavailable)
	}
	if !strings.Contains(logs.String(), "version probe degraded") {
		t.Fatalf("a degraded probe logged nothing actionable: %q", logs.String())
	}
}

func captureWarnings() (*bytes.Buffer, *slog.Logger) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &logs, logger
}

func TestBoundedBufferKeepsAConstantAmountOfMemory(t *testing.T) {
	t.Parallel()
	b := &boundedBuffer{limit: 16}
	chunk := bytes.Repeat([]byte("x"), 1024)
	for range 4096 { // 4MB written, 16 bytes retained
		n, err := b.Write(chunk)
		// A short or failing write makes os/exec abandon the pipe copy, which
		// blocks the child forever on a full pipe.
		if n != len(chunk) || err != nil {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(chunk))
		}
	}
	if len(b.buf) != 16 {
		t.Fatalf("retained %d bytes, want the %d-byte cap", len(b.buf), 16)
	}
	if !b.truncated {
		t.Fatal("truncation went unrecorded, so oversized output would read as a version")
	}
}

func TestBoundedBufferKeepsShortOutputWhole(t *testing.T) {
	t.Parallel()
	b := &boundedBuffer{limit: 16}
	if _, err := b.Write([]byte("1.2.3\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := string(b.buf); got != "1.2.3\n" {
		t.Fatalf("buffer = %q, want the output verbatim", got)
	}
	if b.truncated {
		t.Fatal("output within the cap was marked truncated")
	}
}

func TestCacheToolchainVersionIncludesResolvedRuntimeRequirements(t *testing.T) {
	t.Parallel()

	ext := &extension.ExtensionDescription{RuntimeToolchains: map[string]model.RuntimeToolchainResolution{
		"compiler": {Available: true, Identity: "lock-a"},
	}}
	job := &ScheduledJob{Extension: ext, JobDef: &extension.JobDefinition{Toolchains: []string{"compiler"}}}
	first := cacheToolchainVersion(job)
	ext.RuntimeToolchains["compiler"] = model.RuntimeToolchainResolution{Available: true, Identity: "lock-b"}
	second := cacheToolchainVersion(job)

	if want := runtime.Version() + ";runtime-tools=compiler=lock-a"; first != want {
		t.Fatalf("runtime toolchain key = %q, want %q", first, want)
	}
	if first == second {
		t.Fatalf("runtime toolchain key did not vary after a locked identity change: %q", first)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "identity-keys-results",
		"a-task-cache-key-follows-the-resolved-identity")
}

func TestCacheToolchainVersionLeavesTasksWithoutRequirementsStable(t *testing.T) {
	t.Parallel()

	job := &ScheduledJob{Extension: &extension.ExtensionDescription{Name: "example"}, JobDef: &extension.JobDefinition{}}
	if got := cacheToolchainVersion(job); got != runtime.Version() {
		t.Fatalf("task toolchain cache key = %q, want %q", got, runtime.Version())
	}
}
