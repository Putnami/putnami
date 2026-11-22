//go:build unix

package build

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// linkFrameworkPackages links the workspace's @putnami/application and
// @putnami/runtime into nodeModules, as an install links them into a
// project. It skips the test when the workspace is not installed.
func linkFrameworkPackages(t *testing.T, nodeModules string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no Putnami workspace holds this test")
		}
		dir = parent
	}
	if err := os.MkdirAll(filepath.Join(nodeModules, "@putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"application", "runtime"} {
		source := filepath.Join(dir, "typescript", "framework", name)
		if _, err := os.Stat(filepath.Join(source, "node_modules")); err != nil {
			t.Skipf("@putnami/%s is not installed: run `putnami install`", name)
		}
		if err := os.Symlink(source, filepath.Join(nodeModules, "@putnami", name)); err != nil {
			t.Fatal(err)
		}
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// TestCompiledExecutableListensOnTheConfiguredPort compiles a service whose
// conf/.env.yaml and conf/.env.production.yaml set putnami.port, then runs the
// executable from another directory, as an image does: it listens on the
// base port, never the machine-local conf/.env.local.yaml one, and with
// NODE_ENV=production on the production port. Its src/serve.ts runs too, as
// under `putnami serve`, though the project marks itself "sideEffects": false.
func TestCompiledExecutableListensOnTheConfiguredPort(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "compiled-service-config", "the-compiled-executable-listens-on-the-configured-port")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "compiled-service-config", "the-compile-reads-node-env-at-run-time")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "compiled-service-config", "the-compiled-entry-runs-the-project-serve-entry")
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
	project := filepath.Join(root, "api")
	basePort, productionPort := freeTCPPort(t), freeTCPPort(t)
	files := map[string]string{
		"package.json":              `{"name":"api","private":true,"sideEffects":false,"main":"src/main.ts","exports":{"./serve":"src/serve.ts"}}`,
		"src/main.ts":               "import { application, http } from '@putnami/application';\nexport const app = () => application().use(http());\n",
		"src/serve.ts":              "import { bootstrapServe } from '@putnami/application';\nimport { app } from './main';\nconsole.log('serve entry ran');\nawait bootstrapServe(app);\n",
		"conf/.env.yaml":            fmt.Sprintf("putnami:\n  port: %d\n", basePort),
		"conf/.env.production.yaml": fmt.Sprintf("putnami:\n  port: %d\n", productionPort),
		// A machine-local override stays out of the executable.
		"conf/.env.local.yaml": fmt.Sprintf("putnami:\n  port: %d\n", freeTCPPort(t)),
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
	linkFrameworkPackages(t, filepath.Join(project, "node_modules"))
	manifest := writeCapabilityActivationManifest(t, project, `{"protocolVersion":1,"project":"api"}`)
	entry, err := GenerateBundledServe(project, manifest, nil)
	if err != nil || entry == "" {
		t.Fatalf("GenerateBundledServe = %q, %v; want an entry", entry, err)
	}
	target, err := ImageCompileTarget(runtime.GOOS + "/" + runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	executables, failures, err := RunCompile(bun, project, filepath.Join(root, "out"), entry, []string{target})
	if err != nil || len(failures) > 0 || len(executables) != 1 {
		t.Fatalf("RunCompile = %v, %v, %v; want one executable", executables, failures, err)
	}
	// The project moves away: the executable runs from a directory that holds
	// no conf/, as in an image.
	if err := os.RemoveAll(filepath.Join(project, "conf")); err != nil {
		t.Fatal(err)
	}

	for _, run := range []struct {
		nodeEnv string
		port    int
	}{{"", basePort}, {"production", productionPort}} {
		t.Run("NODE_ENV="+run.nodeEnv, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := stdexec.CommandContext(ctx, executables[0])
			cmd.Dir = t.TempDir()
			var env []string
			for _, kv := range os.Environ() {
				name, _, _ := strings.Cut(kv, "=")
				switch name {
				case "PORT", "NODE_ENV", "APP_ENV", "K_SERVICE", "CONFIG_DATA", "PUTNAMI_PROJECT_ROOT", "PWD":
					continue
				}
				env = append(env, kv)
			}
			if run.nodeEnv != "" {
				env = append(env, "NODE_ENV="+run.nodeEnv)
			}
			cmd.Env = env
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = cmd.Stdout
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}()
			want := fmt.Sprintf("listening http://localhost:%d", run.port)
			var lines []string
			serveEntryRan := false
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				lines = append(lines, line)
				serveEntryRan = serveEntryRan || strings.Contains(line, "serve entry ran")
				if strings.Contains(line, "listening http://") {
					if !strings.Contains(line, want) {
						t.Fatalf("the executable logged %q, want %q", line, want)
					}
					if !serveEntryRan {
						t.Fatalf("the executable listened before src/serve.ts ran:\n%s", strings.Join(lines, "\n"))
					}
					return
				}
			}
			t.Fatalf("the executable never listened:\n%s", strings.Join(lines, "\n"))
		})
	}
}
