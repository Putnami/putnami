package workspacejob_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
)

// The CLI resolves the Go every task runs with from the manifest's
// runtimeCompiler candidates, before any task starts. The release `putnami
// install` installs on a host without Go must be one of them: an environment
// candidate below PUTNAMI_WORKSPACE_ROOT, which the CLI sets to the workspace
// root, names the go command a managed install keeps.
func TestTheManagedGoInstallIsARuntimeCompilerCandidate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Runtime struct {
			Toolchains map[string]struct {
				Candidates []struct {
					From        string `json:"from"`
					Environment string `json:"environment"`
					Path        string `json:"path"`
				} `json:"candidates"`
			} `json:"toolchains"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	ws := jobtest.RealTempDir(t)
	j, _, _ := shellFreeJob(t, ws)
	want := workspacejob.ManagedGoBinary(j.ExtensionStateRoot(), lockedGo)
	var declared []string
	for _, candidate := range manifest.Runtime.Toolchains["runtimeCompiler"].Candidates {
		if candidate.From != "environment" || candidate.Environment != "PUTNAMI_WORKSPACE_ROOT" {
			continue
		}
		// The CLI's expansion: {version} is the locked version, and a path
		// without an extension gains .exe on Windows.
		got := filepath.Join(ws, filepath.FromSlash(strings.ReplaceAll(candidate.Path, "{version}", lockedGo)))
		if runtime.GOOS == "windows" && filepath.Ext(got) == "" {
			got += ".exe"
		}
		if got == want {
			return
		}
		declared = append(declared, got)
	}
	t.Fatalf("no runtimeCompiler candidate names the managed install %s; workspace candidates expand to %v", want, declared)
}
