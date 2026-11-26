package orchestration

import (
	"maps"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// TestATscOutputGitDoesNotIgnoreIsRefusedOnceItKeysTheBuild runs the
// TypeScript counts workspace with a .gitignore that ignores neither dist nor
// the .tsbuildinfo. The build-tsc check reads the candidate cut its key holds
// (typescript/extension ADR 0015): a fresh checkout holds no output, so the
// first gate passes; the next one finds the emitted files in the cut, which
// move the build's key, and refuses the build before tsc runs.
func TestATscOutputGitDoesNotIgnoreIsRefusedOnceItKeysTheBuild(t *testing.T) {
	clitest.RequireShell(t)
	var tc extensionCase
	for _, c := range extensionCases {
		if c.name == "typescript" {
			tc = c
		}
	}
	if tc.name == "" {
		t.Fatal("no typescript extension case")
	}
	tc.files = maps.Clone(tc.files)
	tc.files[".gitignore"] = ".putnami\nnode_modules\n"
	root := prepareExtensionCase(t, t.TempDir(), tc)

	if code, out := clitest.RunGateArgs(t, root, tc.command...); code != 0 {
		t.Fatalf("a fresh checkout: putnami %v exit %d, want 0:\n%s", tc.command, code, out)
	}
	code, out := clitest.RunGateArgs(t, root, tc.command...)
	if code == 0 || !strings.Contains(out, "build~tsc") || !strings.Contains(out, "dist is an output of tsc that Git does not ignore, and it holds dist/") {
		t.Fatalf("the run after the emit: putnami %v exit %d, want build~tsc refused for a dist Git does not ignore:\n%s", tc.command, code, out)
	}
}
