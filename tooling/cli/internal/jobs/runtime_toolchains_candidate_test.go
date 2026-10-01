package jobs

import (
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// A candidate that expands to no path is reported with what is missing and
// with no host path: on Windows the executable suffix is never appended to an
// empty expansion, so the report never reads "at .exe".
func TestRuntimeToolchainUnexpandedCandidatesNameWhatIsMissing(t *testing.T) {
	workspaceRoot := t.TempDir()
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{
		{From: extensionproto.RuntimeToolchainCandidateEnvironment, Environment: "COMPILER_HOME", Path: "bin/compiler"},
		{From: extensionproto.RuntimeToolchainCandidatePath, Path: "compiler"},
	}
	ext := runtimeToolchainExtension(requirement)
	err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"},
		[]string{"PATH=" + t.TempDir(), "COMPILER_HOME= "})
	want := `resolve extension "example" runtime toolchain "compiler": no candidate matched locked version "1.2.3": ` +
		`candidate 1 (environment COMPILER_HOME "bin/compiler"): environment variable COMPILER_HOME is not set; ` +
		`candidate 2 (path "compiler"): no executable on PATH`
	if err == nil || err.Error() != want {
		t.Fatalf("resolution error = %v\nwant %s", err, want)
	}
}

func TestWithExecutableSuffix(t *testing.T) {
	cases := []struct{ goos, path, want string }{
		{"windows", "", ""},
		{"windows", `C:\go\bin\go`, `C:\go\bin\go.exe`},
		{"windows", `C:\go\bin\go.exe`, `C:\go\bin\go.exe`},
		{"linux", "", ""},
		{"linux", "/usr/local/go/bin/go", "/usr/local/go/bin/go"},
	}
	for _, c := range cases {
		if got := withExecutableSuffix(c.goos, c.path); got != c.want {
			t.Errorf("withExecutableSuffix(%q, %q) = %q, want %q", c.goos, c.path, got, c.want)
		}
	}
}
