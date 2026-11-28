package build

import (
	"errors"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
)

// bunPrinting answers `bun --version` with version, or fails it with err.
func bunPrinting(t *testing.T, version string, err error) *int {
	t.Helper()
	probes := 0
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if !slices.Equal(args, []string{"--version"}) {
			t.Errorf("resolving the type-checker ran bun %v", args)
			return &exec.Result{Success: true}, nil
		}
		probes++
		if err != nil {
			return nil, err
		}
		return &exec.Result{Success: true, Stdout: version + "\n"}, nil
	})
	return &probes
}

// Unset, the type-checker is bun check on a Bun that runs it, 1.4.4 or later,
// and tsc on an older Bun, read from the Bun that runs the task.
func TestResolveTypeChecker_DefaultsToBunCheckOnABunThatRunsIt(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-type-checker-choice", "the-type-checker-is-the-parameter-else-bun-check-on-a-bun-that-runs-it")
	for version, want := range map[string]string{
		"1.4.4": TypeCheckerBun, "1.4.5": TypeCheckerBun, "1.5.0": TypeCheckerBun, "2.0.0-canary.1": TypeCheckerBun,
		"1.4.3": TypeCheckerTsc, "1.3.9": TypeCheckerTsc, "1.0.0": TypeCheckerTsc,
	} {
		probes := bunPrinting(t, version, nil)
		got, err := ResolveTypeChecker("", "bun", t.TempDir())
		if err != nil || got != want {
			t.Errorf("on Bun %s the default type-checker = %q, %v, want %q", version, got, err, want)
		}
		if *probes != 1 {
			t.Errorf("on Bun %s the default read the version %d times, want once", version, *probes)
		}
	}
}

// The parameter wins over the default on every Bun, and reads no version.
func TestResolveTypeChecker_TheParameterWinsOverTheDefault(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-type-checker-choice", "the-type-checker-is-the-parameter-else-bun-check-on-a-bun-that-runs-it")
	for _, version := range []string{"1.3.9", "1.4.5"} {
		probes := bunPrinting(t, version, nil)
		for _, param := range []string{TypeCheckerTsc, TypeCheckerBun} {
			if got, err := ResolveTypeChecker(param, "bun", t.TempDir()); err != nil || got != param {
				t.Errorf("on Bun %s, type-checker %q resolved %q, %v", version, param, got, err)
			}
		}
		if *probes != 0 {
			t.Errorf("an explicit type-checker read the Bun version %d times", *probes)
		}
	}
}

// A default whose Bun version cannot be read fails, so one key never holds
// the verdicts of two checkers.
func TestResolveTypeChecker_AnUnreadableBunVersionFails(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-type-checker-choice", "a-bun-without-bun-check-fails-the-task-naming-the-version")
	bunPrinting(t, "", errors.New("exec: bun: not found"))
	if got, err := ResolveTypeChecker("", "bun", t.TempDir()); err == nil {
		t.Fatalf("an unreadable Bun version resolved %q", got)
	}
	bunPrinting(t, "unknown", nil)
	if got, err := ResolveTypeChecker("", "bun", t.TempDir()); err == nil {
		t.Fatalf("a Bun version this does not read resolved %q", got)
	}
}
