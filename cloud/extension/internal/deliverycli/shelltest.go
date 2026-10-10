package deliverycli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// putnami cloud shell-test — the test-verb hook for projects whose subject under
// test is a shell program, so no language extension provides them a `test` job.
// The @putnami/cloud extension manifest activates it per project
// on a committed `tests/run.sh` and runs it with cwd = the project root, so
// `putnami test --impacted` covers those projects alongside the code they ship.
//
// Today that is the CI runner image, whose `ci-run-native.sh` (+ `ci-run-lib.sh`)
// is the entire CI plane's execution contract and whose only gates before this
// hook were `bash -n` and a SECOND project's Go test string-matching its source.
//
// It fails loudly rather than soft-skipping, which the retired `cloud
// image-build` hook did not: that one stood down where docker was missing,
// because a machine without a daemon cannot be asked to build an image and a
// build gate must stay green in a sandbox. A missing test harness is not that —
// the hook's activation marker IS the harness, so its absence means the manifest
// and the tree disagree, which is a loud failure. A test that cannot run must
// never read as a test that passed.

// shellTestHarness is the committed, project-relative harness path. It doubles as
// the extension manifest's activationFiles entry; the two must name the same file
// (cmd/putnami-cloud tests pin the manifest half).
const shellTestHarness = "tests/run.sh"

// Bounds on what a failure re-states. A harness produces hundreds of lines and
// only a handful of them are the verdict, so the failure report quotes the TAP
// assertions that failed rather than the transcript — and quotes a plain tail
// only when there are no such assertions, which is how a harness that died
// before asserting anything still says something.
const (
	// shellTestMaxFailureLines caps the failing assertions re-emitted on the
	// diagnostic channel.
	shellTestMaxFailureLines = 20
	// shellTestMaxTailLines caps the fallback transcript tail, used only when
	// the harness reported no failing assertion at all.
	shellTestMaxTailLines = 20
	// shellTestErrorQuoteLines caps how many of those lines the returned error
	// carries. The error message is rendered inline against a failing task, so
	// it stays shorter than the diagnostic stream it summarizes.
	shellTestErrorQuoteLines = 5
)

// shellTestFailureMarker is the TAP prefix a harness assertion failure carries.
const shellTestFailureMarker = "not ok"

// shellTestLookPath resolves the shell the harness runs under. A package var so
// tests exercise the missing-interpreter path without touching PATH.
var shellTestLookPath = exec.LookPath

// shellTestCapture is the bounded record of one harness run, collected while the
// output streams so a failure can be re-stated afterwards without holding the
// whole transcript. Both slices are capped as they fill: `failures` keeps the
// FIRST failing assertions (the ones that explain the rest) and `tail` keeps the
// LAST lines (where a harness that died mid-run says why).
//
// EVERY field is guarded by mu, because the two fds arrive CONCURRENTLY: os/exec
// copies stdout and stderr on their own goroutines whenever the writers are not
// *os.File, and these are line writers. Unguarded, a harness writing both fds at
// once races the dedup map, and a concurrent map write is a fatal throw the
// runtime does not let anyone recover from — so the failure path would kill the
// process instead of reporting the failure it exists to report. That the same
// structure is only ever populated by a FAILING run is exactly what makes the
// race worth locking rather than reasoning away.
type shellTestCapture struct {
	mu        sync.Mutex
	failures  []string
	seen      map[string]bool
	failed    int
	tail      []string
	truncated bool
}

// observe records one harness output line. It is called for both fds, from the
// goroutine that copies each.
//
// Failing assertions are deduplicated because a harness that already re-states
// its own failures on stderr (the CI runner image's does, so that `bash tests/run.sh`
// stays readable on its own) would otherwise have each of them counted twice
// here — the capture sees both fds.
func (c *shellTestCapture) observe(line string) {
	trimmed := strings.TrimSpace(line)

	c.mu.Lock()
	defer c.mu.Unlock()

	if strings.HasPrefix(trimmed, shellTestFailureMarker) && !c.seen[trimmed] {
		if c.seen == nil {
			c.seen = map[string]bool{}
		}
		c.seen[trimmed] = true
		c.failed++
		if len(c.failures) < shellTestMaxFailureLines {
			c.failures = append(c.failures, trimmed)
		}
	}
	if trimmed == "" {
		return
	}
	if len(c.tail) == shellTestMaxTailLines {
		c.tail = c.tail[1:]
		c.truncated = true
	}
	c.tail = append(c.tail, trimmed)
}

// evidence is the bounded set of lines that explains the failure: the failing
// assertions when the harness reported any, and otherwise the transcript tail.
//
// Held under the same lock as observe. Both copying goroutines have finished by
// the time the caller has an error to report (cmd.Run waits for them), so this
// is not contended in practice — it is locked so the type is safe to read from
// anywhere rather than only from the one call site that happens to be ordered.
func (c *shellTestCapture) evidence() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.failures) > 0 {
		lines := append([]string(nil), c.failures...)
		if c.failed > len(c.failures) {
			lines = append(lines, fmt.Sprintf("… and %d more failing assertion(s)", c.failed-len(c.failures)))
		}
		return lines
	}
	if len(c.tail) == 0 {
		return nil
	}
	lines := append([]string(nil), c.tail...)
	if c.truncated {
		lines = append([]string{"… earlier output omitted"}, lines...)
	}
	return lines
}

// shellTestRun executes the harness, streaming its output line by line through
// the command IO so a long suite is visible as it runs, and capturing the
// bounded evidence a failure needs. A package var so unit tests record the exact
// argv and drive the failure paths without a real suite.
var shellTestRun = func(shell, harness, dir string, ioctx clicore.IO) (*shellTestCapture, error) {
	cmd := exec.Command(shell, harness) //nolint:gosec // G204: argv is the resolved interpreter plus a project-local committed harness path; running it is the feature
	cmd.Dir = dir
	capture := &shellTestCapture{}
	stdout := &imageBuildLineWriter{emit: func(line string) {
		capture.observe(line)
		ioctx.Stdout(line)
	}}
	stderr := &imageBuildLineWriter{emit: func(line string) {
		capture.observe(line)
		ioctx.Stderr(line)
	}}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	stdout.flush()
	stderr.flush()
	return capture, err
}

// shellTestExitStatus recovers the harness's OWN exit status. The harness spends
// that status as a channel — the CI runner image encodes the failing suite's
// index into it — so collapsing it to the CLI's generic failure code throws away the one piece
// of the verdict that survives every renderer. Returns -1 when the run failed
// without producing a status (a signal, or a failure to start at all).
func shellTestExitStatus(err error) int {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}

// shellTestFailureSummary is the one-line verdict, naming the harness and its
// real exit status.
func shellTestFailureSummary(harness string, status int) string {
	if status < 0 {
		return fmt.Sprintf("cloud shell-test: %s failed without an exit status", harness)
	}
	return fmt.Sprintf("cloud shell-test: %s failed with exit status %d", harness, status)
}

// ShellTest backs `putnami cloud shell-test`: run the current project's
// `tests/run.sh` and take its exit status as the verdict.
//
// Params (all optional):
//
//	project — project directory override (default: the project the CLI resolved
//	          for this task, else the process cwd)
func ShellTest(params map[string]any, _ []string, workspaceRoot string, _ map[string]string, ioctx clicore.IO) error {
	dir, err := shellTestProjectDir(params, workspaceRoot)
	if err != nil {
		return clicore.NewError("cloud shell-test: resolve project: "+err.Error(), clicore.ExitUsage)
	}
	harness := filepath.Join(dir, shellTestHarness)
	if _, statErr := os.Stat(harness); statErr != nil {
		// Fail, never skip: the manifest only schedules this task for a project
		// that HAS the harness, so a missing one means the plan and the tree
		// disagree — exactly the class of drift that makes a gate report green
		// without running.
		return clicore.NewError(
			"cloud shell-test: no test harness at "+harness+" — this hook activates on "+shellTestHarness+", so its absence is a manifest/tree drift, not a skip",
			clicore.ExitUsage)
	}

	shell, lookErr := shellTestLookPath("bash")
	if lookErr != nil {
		return clicore.NewError("cloud shell-test: bash is not on PATH; the harness cannot run", clicore.ExitFailure)
	}
	capture, runErr := shellTestRun(shell, harness, dir, ioctx)
	if runErr != nil {
		return shellTestFailure(harness, capture, runErr, ioctx)
	}

	out := map[string]any{"status": "passed", "harness": harness, "project": dir}
	clicore.WriteResult(out, params, ioctx, fmt.Sprintf("Shell test harness %s passed.", harness))
	return nil
}

// shellTestFailure reports a failing harness on the two channels that survive
// the CLI's machine mode, and returns the job's verdict.
//
// WHY A FAILURE IS RESTATED AT ALL. The harness's transcript streams through
// ioctx.Stdout, which machine mode renders as INFO log events. Those reach a
// jsonl consumer, but every failure renderer filters them out — a task that
// failed is described by its diagnostics and its error, not by the hundreds of
// lines it happened to print. So a failure that says nothing beyond those lines
// is a failure nobody can read from CI, which is exactly what this hook shipped:
// ~900 harness lines produced and `exit status 1` recorded, nothing else on any
// surface. The evidence is therefore re-stated ONCE, bounded, on the diagnostic
// channel an extension's own warnings use, and summarized again in the returned
// error so the recorded message names the harness's real exit status.
//
// The returned error is exit-coded ExitFailure, not the harness's status: the
// process exit-code taxonomy (protocol/cli exit.go) is a stable contract that
// scripts and agents branch on, so a harness exiting 2 or 3 must not reach the
// caller as "usage" or "auth". The real status travels in the message instead,
// which is where a reader needs it.
func shellTestFailure(harness string, capture *shellTestCapture, runErr error, ioctx clicore.IO) error {
	status := shellTestExitStatus(runErr)
	summary := shellTestFailureSummary(harness, status)

	var evidence []string
	if capture != nil {
		evidence = capture.evidence()
	}

	ioctx.Stderr(summary)
	for _, line := range evidence {
		ioctx.Stderr(line)
	}

	quoted := evidence
	if len(quoted) > shellTestErrorQuoteLines {
		quoted = quoted[:shellTestErrorQuoteLines]
	}
	var message strings.Builder
	message.WriteString(summary)
	for _, line := range quoted {
		message.WriteString("\n  " + line)
	}
	if len(evidence) > len(quoted) {
		fmt.Fprintf(&message, "\n  … %d more line(s) on the diagnostic channel", len(evidence)-len(quoted))
	}
	return clicore.NewError(message.String(), clicore.ExitFailure)
}

// shellTestProjectDir resolves the project directory the harness runs in, with
// the same precedence ImageBuild uses and for the same reason: the runtime runs
// in the task's declared cwd, not the caller's, so the process cwd is only
// trusted when the CLI resolved no project at all.
func shellTestProjectDir(params map[string]any, workspaceRoot string) (string, error) {
	if override := clicore.StringParam(params, "project"); override != "" {
		return filepath.Abs(override)
	}
	if app := clicore.StringParam(params, "app", "appName"); app != "" && workspaceRoot != "" {
		dir, err := clicore.FindAppDir(workspaceRoot, app)
		if err != nil {
			return "", fmt.Errorf("locate project %q under %s: %w", app, workspaceRoot, err)
		}
		return dir, nil
	}
	return filepath.Abs(".")
}
