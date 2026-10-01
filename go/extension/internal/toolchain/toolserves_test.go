package toolchain

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A tool built with the local Go minor or a newer one serves the workspace; a
// tool built with an older minor panics type-checking it ("package requires
// newer Go version go1.26 (application built with go1.25)").
func TestToolServesLocalGo(t *testing.T) {
	for _, tc := range []struct {
		built, local string
		want         bool
	}{
		{"go1.26.1", "1.25.7", true},   // a release tool on a workspace that pins an older Go
		{"go1.26.1", "go1.26.0", true}, // the same minor, another patch
		{"go1.26.1 X:boringcrypto", "1.26.1", true},
		{"go1.26rc1", "1.26.0", true},
		{"go1.100.0", "1.99.1", true}, // numeric, not lexical
		{"go1.25.7", "1.26.1", false}, // an older tool refuses newer sources
		{"go1.25.7", "1.100.0", false},
		{"devel go1.27-0123abcd Tue Sep 1 00:00:00 2026 +0000", "1.26.1", false},
		{"", "1.26.1", false},
		{"go1.26.1", "", false},
		{"go1.26.1", "unknown", false},
	} {
		if got := ToolServesLocalGo(tc.built, tc.local); got != tc.want {
			t.Errorf("ToolServesLocalGo(%q, %q) = %v, want %v", tc.built, tc.local, got, tc.want)
		}
	}
}

// goMinorOf returns N of a "go1.N..." version.
func goMinorOf(t *testing.T, version string) int {
	t.Helper()
	lang := goLanguageVersion(version)
	minor, err := strconv.Atoi(strings.TrimPrefix(lang, "go1."))
	if err != nil {
		t.Fatalf("no minor in Go version %q", version)
	}
	return minor
}

// A release builds its tools with a newer Go than a workspace may pin: the
// prebuilt artifact serves that workspace, so install restores it instead of
// compiling the tool, and lint keeps the copy install restored. A local Go
// newer than the artifact's still compiles. The first half failed while the
// rule required the same minor: every fresh workspace on the default Go
// compiled both tools, about 4.8 minutes on a 4-vCPU Windows host.
func TestTheArtifactServesALocalGoUpToItsOwnMinor(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not available: %v", err)
	}
	artifact := buildProbeToolFromProxy(t, "example.com/probe", "v1.2.3")
	info, err := buildinfo.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	minor := goMinorOf(t, info.GoVersion)

	clearManagedGoResolutionEnv(t)
	useLocalGo := func(version string) {
		goRoot := t.TempDir()
		writeFakeGoCompiler(t, filepath.Join(goRoot, "bin", goBinaryName()), version)
		t.Setenv("GOROOT", goRoot)
		t.Setenv("PATH", filepath.Join(goRoot, "bin"))
	}
	dest := filepath.Join(t.TempDir(), "tools", toolBinaryName("probe"))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}

	older := fmt.Sprintf("go1.%d.7", minor-1)
	useLocalGo(older)
	if err := installToolFromArtifact(artifact, dest, "probe", "v1.2.3"); err != nil {
		t.Fatalf("an artifact built with %s was refused on a machine that runs %s: %v", info.GoVersion, older, err)
	}
	if !toolBuildMatchesCurrentGoVersion(dest) {
		t.Fatalf("lint refuses the copy install restored for %s", older)
	}

	newer := fmt.Sprintf("go1.%d.0", minor+1)
	useLocalGo(newer)
	err = installToolFromArtifact(artifact, dest, "probe", "v1.2.3")
	if err == nil {
		t.Fatalf("an artifact built with %s was accepted on a machine that runs the newer %s", info.GoVersion, newer)
	}
	if !strings.Contains(err.Error(), info.GoVersion) || !strings.Contains(err.Error(), newer) {
		t.Errorf("refusal = %q, want both Go versions", err)
	}
	if toolBuildMatchesCurrentGoVersion(dest) {
		t.Errorf("lint keeps a tool built with %s on a machine that runs the newer %s", info.GoVersion, newer)
	}
}
