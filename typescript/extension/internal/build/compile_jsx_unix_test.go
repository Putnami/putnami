//go:build unix

package build

import (
	"context"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// testWorkspaceRoot is the Putnami workspace that holds this test. It skips
// the test when none does.
func testWorkspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no Putnami workspace holds this test")
		}
		dir = parent
	}
}

// linkReact links the workspace's installed react into nodeModules. It skips
// the test when no installed framework package holds react.
func linkReact(t *testing.T, nodeModules string) {
	t.Helper()
	root := testWorkspaceRoot(t)
	for _, holder := range []string{"web", "ui"} {
		source := filepath.Join(root, "typescript", "framework", holder, "node_modules", "react")
		if _, err := os.Stat(filepath.Join(source, "package.json")); err != nil {
			continue
		}
		if err := os.MkdirAll(nodeModules, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(source, filepath.Join(nodeModules, "react")); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Skip("react is not installed: run `putnami install`")
}

// fakeNodeEnvBun records the NODE_ENV each `bun build` runs with.
const fakeNodeEnvBun = `#!/bin/sh
if [ "$1" = "--version" ]; then echo 1.4.5; exit 0; fi
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--outfile" ]; then out="$2"; fi
  shift
done
echo "${NODE_ENV-unset}" >> "$FAKE_BUN_NODE_ENV"
echo binary > "$out"
`

// TestRunCompileRunsBunWithProductionNodeEnv guards the wiring of compileEnv:
// a builder that runs in development, as `putnami serve` and most shells do,
// still compiles with NODE_ENV=production.
func TestRunCompileRunsBunWithProductionNodeEnv(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "compiled-service-config", "the-compile-reads-node-env-at-run-time")
	bunBin := filepath.Join(t.TempDir(), "bun")
	if err := os.WriteFile(bunBin, []byte(fakeNodeEnvBun), 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "node-env")
	t.Setenv("FAKE_BUN_NODE_ENV", record)
	t.Setenv("NODE_ENV", "development")

	project := t.TempDir()
	files, errs, err := RunCompile(bunBin, project, filepath.Join(t.TempDir(), "compile"), "src/serve.tsx", []string{"bun-linux-x64", "bun-linux-arm64"})
	if err != nil || len(errs) > 0 || len(files) != 2 {
		t.Fatalf("RunCompile = %v, %v, %v; want two executables", files, errs, err)
	}
	seen, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the fake bun recorded no compile: %v", err)
	}
	if got, want := string(seen), "production\nproduction\n"; got != want {
		t.Fatalf("bun build --compile ran with NODE_ENV %q, want %q", got, want)
	}
}

// TestCompiledExecutableRendersJSXUnderProductionNodeEnv compiles an entry
// that builds a JSX element, from a builder whose NODE_ENV is unset and from
// one in development, then runs the executable as an image does, with
// NODE_ENV=production, and without NODE_ENV. react/jsx-dev-runtime exports
// no jsxDEV under production, so an executable compiled to jsxDEV calls exits
// at its first element.
func TestCompiledExecutableRendersJSXUnderProductionNodeEnv(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "compiled-service-config", "the-compile-reads-node-env-at-run-time")
	if testing.Short() {
		t.Skip("compiles an executable")
	}
	bun, err := stdexec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "site")
	files := map[string]string{
		"package.json":  `{"name":"site","private":true}`,
		"tsconfig.json": `{"compilerOptions":{"jsx":"react-jsx"}}`,
		"src/serve.tsx": "const element = <svg><title>Info</title></svg>;\n" +
			"console.log(`rendered ${element.type} under NODE_ENV=${process.env.NODE_ENV ?? ''}`);\n",
	}
	for rel, body := range files {
		path := filepath.Join(project, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	linkReact(t, filepath.Join(project, "node_modules"))
	target, err := ImageCompileTarget(runtime.GOOS + "/" + runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}

	for _, builderEnv := range []string{"", "development"} {
		t.Run("builder NODE_ENV="+builderEnv, func(t *testing.T) {
			// The builder runs as a workstation or a CI runner does.
			t.Setenv("NODE_ENV", builderEnv)
			if builderEnv == "" {
				os.Unsetenv("NODE_ENV")
			}
			executables, failures, err := RunCompile(bun, project, filepath.Join(t.TempDir(), "out"), "src/serve.tsx", []string{target})
			if err != nil || len(failures) > 0 || len(executables) != 1 {
				t.Fatalf("RunCompile = %v, %v, %v; want one executable", executables, failures, err)
			}
			for _, nodeEnv := range []string{"production", ""} {
				t.Run("NODE_ENV="+nodeEnv, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					cmd := stdexec.CommandContext(ctx, executables[0])
					cmd.Dir = t.TempDir()
					var env []string
					for _, kv := range os.Environ() {
						if name, _, _ := strings.Cut(kv, "="); name != "NODE_ENV" {
							env = append(env, kv)
						}
					}
					if nodeEnv != "" {
						env = append(env, "NODE_ENV="+nodeEnv)
					}
					cmd.Env = env
					output, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("the executable failed: %v\n%s", err, output)
					}
					// The run-time NODE_ENV, not the builder's, reaches the code.
					if want := "rendered svg under NODE_ENV=" + nodeEnv + "\n"; string(output) != want {
						t.Fatalf("the executable printed %q, want %q", output, want)
					}
				})
			}
		})
	}
}
