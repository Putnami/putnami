package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/jsonl"
)

// The dev `go run` path resolves modules: the runner must strip a leaked
// GOWORK=off and re-point GOWORK at the governing go.work, or a workspace
// dependency required as the v0.0.0 placeholder (with no per-module replace)
// escapes to the proxy. With GOPROXY=off the escape fails deterministically, so
// `go run` completes only because the runner re-pointed GOWORK.
func TestRunCommand_RepointsLeakedGoWorkForGoRun(t *testing.T) {
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
	os.WriteFile(filepath.Join(appDir, "go.mod"),
		[]byte("module example.com/app\n\ngo 1.25\n\nrequire example.com/dep v0.0.0\n"), 0o644)
	os.WriteFile(filepath.Join(appDir, "main.go"),
		[]byte("package main\n\nimport \"example.com/dep\"\n\nfunc main() { _ = dep.Hello() }\n"), 0o644)

	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "")

	// serveRunEnv(false, ...) is the env the dev branch of Run threads into the
	// runner; it must re-point GOWORK so `go run` resolves the workspace dep.
	code := runCommand(
		[]string{goBin, "run", "."},
		appDir,
		serveRunEnv(false, appDir, goBin),
		jsonl.New(),
	)
	if code != 0 {
		t.Fatalf("runCommand(go run) exit = %d, want 0: a leaked GOWORK=off must be re-pointed so the workspace dep resolves locally", code)
	}
}

// The production serve path runs a pre-built binary and must inherit the user's
// environment verbatim. serveRunEnv must return nil there so runCommand leaves
// cmd.Env unset (inherit parent) rather than stripping a user-supplied GOWORK or
// any other var the server / its Go tooling relies on.
func TestServeRunEnv_ProdInheritsParentEnv(t *testing.T) {
	if env := serveRunEnv(true, t.TempDir(), ""); env != nil {
		t.Fatalf("serveRunEnv(isProd=true) = %v, want nil so the prod binary inherits the parent environment verbatim", env)
	}
}

// The dev `go run` path resolves modules, so serveRunEnv must strip a leaked
// GOWORK=off and re-point GOWORK at the governing go.work.
func TestServeRunEnv_DevRepointsLeakedGoWorkOff(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gowork := filepath.Join(root, "go.work")
	if err := os.WriteFile(gowork, []byte("go 1.25\n\nuse ./app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GOWORK", "off")

	env := serveRunEnv(false, appDir, "")
	var got string
	found := false
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GOWORK="); ok {
			got = v
			found = true
		}
	}
	if !found {
		t.Fatalf("serveRunEnv(isProd=false) did not set GOWORK; a leaked GOWORK=off must be re-pointed at %s", gowork)
	}
	if got == "off" {
		t.Fatalf("serveRunEnv(isProd=false) left GOWORK=off; want it re-pointed at %s", gowork)
	}
	if got != gowork {
		t.Fatalf("serveRunEnv(isProd=false) GOWORK = %q, want %q", got, gowork)
	}
}
