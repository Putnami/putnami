package artifactstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// cliTestSHA is a valid 64-char lowercase-hex digest. AdmitCLI delegates content
// verification to the StageFunc, so the staged bytes need not actually hash to
// it for these store-level tests.
const cliTestSHA = "1111111111111111111111111111111111111111111111111111111111111111"

// writeCLIStub writes an executable "putnami" with the given content into dir.
func writeCLIStub(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, cliBinaryName), []byte(content), 0o755); err != nil {
		t.Fatalf("write staged putnami: %v", err)
	}
}

func TestHasCLIAndPaths(t *testing.T) {
	s := New(t.TempDir())

	if s.HasCLI(cliTestSHA) {
		t.Fatal("HasCLI true before any admit")
	}
	if s.HasCLI("not-a-digest") {
		t.Error("HasCLI must reject a non-hex digest")
	}

	want := filepath.Join(s.Root(), "cli", cliTestSHA, cliBinaryNameFor(runtime.GOOS))
	if got := s.CLIBinary(cliTestSHA); got != want {
		t.Errorf("CLIBinary = %q, want %q", got, want)
	}

	if err := os.MkdirAll(s.CLIDir(cliTestSHA), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIStub(t, s.CLIDir(cliTestSHA), "binary")
	if !s.HasCLI(cliTestSHA) {
		t.Error("HasCLI false after publishing the binary")
	}
}

// Windows starts only a program whose name ends in ".exe", so a CLI entry
// there holds putnami.exe. Every other OS keeps putnami, the name putnamiw
// publishes, so the Go launcher and the wrapper share one blob.
func TestCLIBinaryNameIsThePlatformExecutableName(t *testing.T) {
	for goos, want := range map[string]string{"windows": "putnami.exe", "linux": "putnami", "darwin": "putnami"} {
		if got := cliBinaryNameFor(goos); got != want {
			t.Errorf("cliBinaryNameFor(%q) = %q, want %q", goos, got, want)
		}
	}
	s := New(t.TempDir())
	if got := filepath.Base(s.CLIBinary(cliTestSHA)); got != CLIBinaryName() || got != cliBinaryNameFor(runtime.GOOS) {
		t.Errorf("CLIBinary file = %q, want this machine's name %q", got, cliBinaryNameFor(runtime.GOOS))
	}
}

func TestAdmitCLI(t *testing.T) {
	s := New(t.TempDir())

	calls := 0
	stage := func(dir string) error {
		calls++
		writeCLIStub(t, dir, "the-cli")
		return nil
	}

	path, err := s.AdmitCLI(cliTestSHA, stage)
	if err != nil {
		t.Fatalf("AdmitCLI: %v", err)
	}
	if path != s.CLIBinary(cliTestSHA) {
		t.Errorf("AdmitCLI path = %q, want %q", path, s.CLIBinary(cliTestSHA))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("admitted binary missing: %v", err)
	}
	// Windows has no execute permission bit; putnami.exe runs by its name.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Error("admitted binary is not executable")
	}
	if !s.HasCLI(cliTestSHA) {
		t.Error("HasCLI false after AdmitCLI")
	}

	// First-writer-wins: a second admit of the same sha must not re-stage.
	if _, err := s.AdmitCLI(cliTestSHA, stage); err != nil {
		t.Fatalf("second AdmitCLI: %v", err)
	}
	if calls != 1 {
		t.Errorf("stage ran %d times, want 1 (second admit must short-circuit)", calls)
	}
}

func TestAdmitCLIInvalidDigest(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.AdmitCLI("../escape", func(string) error { return nil }); err == nil {
		t.Error("AdmitCLI must reject an invalid digest")
	}
}

func TestAdmitCLIStageErrorPublishesNothing(t *testing.T) {
	s := New(t.TempDir())
	wantErr := errors.New("verification failed")
	if _, err := s.AdmitCLI(cliTestSHA, func(string) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("AdmitCLI error = %v, want %v", err, wantErr)
	}
	if s.HasCLI(cliTestSHA) {
		t.Error("a failed stage must publish nothing")
	}
}

func TestTouchCLI(t *testing.T) {
	s := New(t.TempDir())

	s.TouchCLI(cliTestSHA) // no-op on a missing entry

	if _, err := s.AdmitCLI(cliTestSHA, func(dir string) error {
		writeCLIStub(t, dir, "x")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	used := filepath.Join(s.CLIDir(cliTestSHA), lastUsedFile)
	if err := os.Remove(used); err != nil {
		t.Fatalf("remove recency sidecar: %v", err)
	}
	s.TouchCLI(cliTestSHA)
	if _, err := os.Stat(used); err != nil {
		t.Errorf("TouchCLI did not write the recency sidecar: %v", err)
	}
}
