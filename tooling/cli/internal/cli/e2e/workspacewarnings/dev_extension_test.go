package workspacewarnings

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// devToolingDir is the absolute tooling/dev directory. It resolves from this
// package's directory, where the test binary starts, before a test changes
// the working directory.
var devToolingDir = mustAbs(filepath.Join("..", "..", "..", "..", "..", "dev"))

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}

// devRuntimeBuild is the @putnami/dev executable built once for this test
// binary, in devRuntimeDir, which TestMain removes.
var (
	devRuntimeBuild sync.Once
	devRuntimeDir   string
	devRuntimePath  string
	devRuntimeErr   error
)

// devRuntime returns the @putnami/dev executable, built from tooling/dev as
// tooling/dev/bin/prepare builds it.
func devRuntime(t *testing.T) string {
	t.Helper()
	devRuntimeBuild.Do(func() {
		devRuntimeDir, devRuntimeErr = os.MkdirTemp("", "putnami-dev-runtime-")
		if devRuntimeErr != nil {
			return
		}
		devRuntimePath = filepath.Join(devRuntimeDir, "putnami-dev")
		if runtime.GOOS == "windows" {
			devRuntimePath += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", devRuntimePath, "./cmd/putnami-dev")
		cmd.Dir = devToolingDir
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			devRuntimeErr = fmt.Errorf("%w\n%s", err, out)
		}
	})
	if devRuntimeErr != nil {
		t.Fatalf("build the @putnami/dev runtime: %v", devRuntimeErr)
	}
	return devRuntimePath
}

// removeDevRuntime removes the executable devRuntime built, if any.
func removeDevRuntime() {
	if devRuntimeDir != "" {
		_ = os.RemoveAll(devRuntimeDir)
	}
}

// devManifest is the shipped @putnami/dev manifest with its runtime replaced
// by the executable at runtimePath: no runtime to prepare, and every task
// that ran the prepared runtime runs that executable.
func devManifest(t *testing.T, runtimePath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(devToolingDir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read the shipped @putnami/dev manifest: %v", err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	delete(manifest, "runtime")
	var tasks map[string]map[string]json.RawMessage
	if err := json.Unmarshal(manifest["tasks"], &tasks); err != nil {
		t.Fatal(err)
	}
	command, err := json.Marshal(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if string(task["command"]) == `"{extensionRuntime}"` {
			task["command"] = command
		}
	}
	if manifest["tasks"], err = json.Marshal(tasks); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// devRoutes is /app's committed route inventory: one exact, public GET route,
// from which qualify derives one smoke request.
const devRoutes = `{
  "$schema": "https://putnami.dev/schemas/putnami-http-routes-v1.json",
  "protocol": "putnami.http-routes.v1",
  "routes": [
    {
      "match": "exact",
      "path": "/items",
      "methods": [
        "GET"
      ],
      "publicEdge": true,
      "provenance": {
        "project": "app",
        "sourceKind": "manual"
      }
    }
  ],
  "digest": "sha256:8d957da3be380f191d6329b5aca64ec414b207933dd26b5d978d84760a4b7a17"
}
`

// devWorkspace writes a workspace that holds the @putnami/dev extension,
// which runs compose and qualify, and one project, /app, which commits a
// route inventory with one safe route. When declaresJobs is true, its
// putnami.json declares a `jobs` key, which the load warns about.
func devWorkspace(t *testing.T, declaresJobs bool) string {
	t.Helper()
	root := t.TempDir()
	project := `{"name":"app"}`
	if declaresJobs {
		project = `{"name":"app","jobs":{}}`
	}
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"dev-warnings","includes":["dev-extension","app"]}`)
	clitest.WriteFile(t, filepath.Join(root, "dev-extension", "putnami.json"), `{"name":"@putnami/dev"}`)
	clitest.WriteFile(t, filepath.Join(root, "dev-extension", "putnami.extension.json"), devManifest(t, devRuntime(t)))
	clitest.WriteFile(t, filepath.Join(root, "app", "putnami.json"), project)
	clitest.WriteFile(t, filepath.Join(root, "app", "schema", "http-routes.json"), devRoutes)
	return root
}
