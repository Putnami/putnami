package run

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.putnami.dev/go/extension/internal/platform"
	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestRunOnce_ForwardsZeroExit(t *testing.T) {
	code := runOnce([]string{"sh", "-c", "echo hello; exit 0"}, t.TempDir(), jsonl.New())
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunOnce_ForwardsNonZeroExit(t *testing.T) {
	// The whole point of `run`: a workload exiting 3 must surface as 3, not a
	// generic failure, so callers can assert specific gate exit codes.
	code := runOnce([]string{"sh", "-c", "exit 3"}, t.TempDir(), jsonl.New())
	if code != 3 {
		t.Fatalf("expected exit 3 to be forwarded, got %d", code)
	}
}

func TestRunOnce_ForwardsStderrAndExit(t *testing.T) {
	code := runOnce([]string{"sh", "-c", "echo boom 1>&2; exit 1"}, t.TempDir(), jsonl.New())
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
}

func TestRunOnce_StartFailureReturnsOne(t *testing.T) {
	code := runOnce([]string{"definitely-not-a-real-binary-xyz"}, t.TempDir(), jsonl.New())
	if code != 1 {
		t.Fatalf("expected exit 1 for unstartable command, got %d", code)
	}
}

// TestBuildAndRun_ForwardsExactExitCode is the regression guard for the core
// issue: a compiled binary that exits 3 must surface as 3 (go run would
// collapse it to 1).
func TestBuildAndRun_ForwardsExactExitCode(t *testing.T) {
	goBin, err := toolchain.ResolveGo()
	if err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\nimport \"os\"\nfunc main(){ os.Exit(3) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	binPath := filepath.Join(t.TempDir(), platform.HostBinaryName(runtime.GOOS, ".", "probe"))
	if !buildHostBinary(goBin, binPath, ".", false, dir, jsonl.New()) {
		t.Fatal("expected build to succeed")
	}
	if code := runOnce([]string{binPath}, dir, jsonl.New()); code != 3 {
		t.Fatalf("expected exact exit 3 from compiled binary, got %d", code)
	}
}

// TestRun_StartsTheBinaryItBuildsOnThisHost drives the whole job: the file Run
// builds must be one this host starts. On Windows a name without .exe builds
// fine and then fails to start with "executable file not found", which Run
// reports as exit 1 instead of the workload's own code.
func TestRun_StartsTheBinaryItBuildsOnThisHost(t *testing.T) {
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\nimport \"os\"\nfunc main(){ os.Exit(3) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := &pctx.Context{Project: pctx.Project{Name: "probe", FullPath: dir}, Params: pctx.Params{}}
	status, data, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "FAILED" || data[exitCodeKey] != 3 {
		t.Fatalf("Run = %q with data %v, want FAILED with the workload's exit code 3", status, data)
	}
}

// A leaked GOWORK=off must not reach the host-binary build. buildHostBinary must
// re-point GOWORK at the governing go.work so a workspace dependency required as
// the v0.0.0 placeholder (with no per-module replace) resolves locally through
// the workspace `use` rather than escaping to the proxy. With GOPROXY=off the
// escape fails deterministically, so this build succeeds only because the leaked
// GOWORK=off was stripped and re-pointed.
func TestBuildHostBinary_RepointsLeakedGoWorkOff(t *testing.T) {
	goBin, err := toolchain.ResolveGo()
	if err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}

	// Canonicalize the temp root: on macOS t.TempDir() is under a /var/folders
	// symlink, and go rejects a go.work whose relative `use` paths don't match the
	// subprocess's symlink-resolved cwd ("not one of the workspace modules").
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.work"),
		[]byte("go 1.25\n\nuse (\n\t./dep\n\t./app\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	depDir := filepath.Join(root, "dep")
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(depDir, "go.mod"), []byte("module example.com/dep\n\ngo 1.25\n"), 0o644)
	os.WriteFile(filepath.Join(depDir, "dep.go"), []byte("package dep\n\nfunc Hello() string { return \"hi\" }\n"), 0o644)
	// app requires dep as the v0.0.0 placeholder with NO per-module replace: only
	// the go.work `use` resolves it, so a leaked GOWORK=off would send it to the
	// proxy.
	os.WriteFile(filepath.Join(appDir, "go.mod"),
		[]byte("module example.com/app\n\ngo 1.25\n\nrequire example.com/dep v0.0.0\n"), 0o644)
	os.WriteFile(filepath.Join(appDir, "main.go"),
		[]byte("package main\n\nimport \"example.com/dep\"\n\nfunc main() { _ = dep.Hello() }\n"), 0o644)

	// Simulate the leaked standalone env; GOPROXY=off makes a v0.0.0 escape fail
	// deterministically rather than reach the network.
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "")

	binPath := filepath.Join(t.TempDir(), "app")
	if !buildHostBinary(goBin, binPath, ".", false, appDir, jsonl.New()) {
		t.Fatal("buildHostBinary should succeed: a leaked GOWORK=off must be re-pointed at the governing go.work so the workspace dep resolves locally")
	}
}

func TestBuildHostBinary_CompileErrorReturnsFalse(t *testing.T) {
	goBin, err := toolchain.ResolveGo()
	if err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module bad\n\ngo 1.25\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() { this is not go }\n"), 0o644)

	if buildHostBinary(goBin, filepath.Join(t.TempDir(), "x"), ".", false, dir, jsonl.New()) {
		t.Fatal("expected build to fail on a compile error")
	}
}
