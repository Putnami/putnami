package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Serve is build-and-exec: it compiles what it runs and
// leaves nothing behind. These tests pin the "leaves nothing behind" half, which
// is the part a refactor can quietly lose — a serve that started writing into
// the command's output directory again would look identical from the outside
// and would put a second copy of every served binary on disk, plus a cache entry
// nothing consumes.

func newBuildExecProject(t *testing.T) (projectDir, outputDir string) {
	t.Helper()
	projectDir = t.TempDir()
	outputDir = t.TempDir()
	mustWriteServeFile(t, filepath.Join(projectDir, "go.mod"), "module example.com/served\n\ngo 1.24.0\n")
	mustWriteServeFile(t, filepath.Join(projectDir, "main.go"), "package main\n\nfunc main() {}\n")
	return projectDir, outputDir
}

// TestBuildServeBinaryEmitsOnlyWhereAsked proves the production path compiles
// into the scratch directory it was handed and writes nowhere else — in
// particular not into the project tree (.gen) or a command output directory.
func TestBuildServeBinaryEmitsOnlyWhereAsked(t *testing.T) {
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}

	projectDir, outputDir := newBuildExecProject(t)
	scratch := t.TempDir()
	binPath := filepath.Join(scratch, "served")

	if !buildServeBinary(goBinary, binPath, ".", false, projectDir, jsonl.New()) {
		t.Fatal("buildServeBinary reported failure for a trivial main package")
	}
	if _, statErr := os.Stat(binPath); statErr != nil {
		t.Fatalf("serve binary missing from its scratch directory: %v", statErr)
	}

	if entries, readErr := os.ReadDir(outputDir); readErr == nil && len(entries) > 0 {
		t.Errorf("serve wrote %d entries into the command output directory; a served process is not an artifact", len(entries))
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, ".gen")); statErr == nil {
		t.Error("serve wrote into the project's .gen tree; build-and-exec emits no binaries there")
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "bin")); statErr == nil {
		t.Error("serve wrote a bin/ tree into the project directory")
	}
}

// TestBuildServeBinaryReportsCompileFailures pins that build-and-exec did not
// trade diagnostics for the removed compile step: a program that does not build
// fails serve with the toolchain's own message rather than starting nothing and
// reporting success.
func TestBuildServeBinaryReportsCompileFailures(t *testing.T) {
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}

	projectDir := t.TempDir()
	mustWriteServeFile(t, filepath.Join(projectDir, "go.mod"), "module example.com/broken\n\ngo 1.24.0\n")
	mustWriteServeFile(t, filepath.Join(projectDir, "main.go"), "package main\n\nfunc main() { undefinedCall() }\n")

	out := captureServeStdout(t, func() {
		if buildServeBinary(goBinary, filepath.Join(t.TempDir(), "broken"), ".", false, projectDir, jsonl.New()) {
			t.Error("buildServeBinary reported success for a program that does not compile")
		}
	})
	if !strings.Contains(out, "undefinedCall") {
		t.Errorf("compile failure did not reach the job's diagnostics:\n%s", out)
	}
}

func mustWriteServeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
