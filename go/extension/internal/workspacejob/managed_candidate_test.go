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
// install` installs on a host without Go must be one of them: the putnami-home
// candidate names the go command of the install under the Putnami home. A
// later environment candidate below PUTNAMI_WORKSPACE_ROOT, which the CLI sets
// to the workspace root, names the go command of a release installed inside
// the workspace, so the home install wins when a workspace holds both.
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
	putnamiHome := jobtest.RealTempDir(t)
	j, _, _ := shellFreeJobAt(t, ws, putnamiHome)
	homeInstall := workspacejob.ManagedGoBinary(j.GoToolchainRoot(), lockedGo)
	workspaceInstall := workspacejob.ManagedGoBinary(j.WorkspaceGoRoot(), lockedGo)

	// The CLI's expansion: {version} is the locked version, a putnami-home
	// path is below the Putnami home, an environment path is below the
	// variable's value, and a path without an extension gains .exe on Windows.
	homeAt, workspaceAt := -1, -1
	candidates := manifest.Runtime.Toolchains["runtimeCompiler"].Candidates
	declared := make([]string, 0, len(candidates))
	for index, candidate := range candidates {
		var base string
		switch {
		case candidate.From == "putnami-home":
			base = putnamiHome
		case candidate.From == "environment" && candidate.Environment == "PUTNAMI_WORKSPACE_ROOT":
			base = ws
		default:
			continue
		}
		got := filepath.Join(base, filepath.FromSlash(strings.ReplaceAll(candidate.Path, "{version}", lockedGo)))
		if runtime.GOOS == "windows" && filepath.Ext(got) == "" {
			got += ".exe"
		}
		declared = append(declared, got)
		switch got {
		case homeInstall:
			homeAt = index
		case workspaceInstall:
			workspaceAt = index
		}
	}
	if homeAt < 0 {
		t.Fatalf("no runtimeCompiler candidate names the install under the Putnami home %s; candidates expand to %v", homeInstall, declared)
	}
	if workspaceAt < 0 {
		t.Fatalf("no runtimeCompiler candidate names the install inside the workspace %s; candidates expand to %v", workspaceInstall, declared)
	}
	if homeAt > workspaceAt {
		t.Fatalf("the workspace candidate (%d) comes before the Putnami home candidate (%d)", workspaceAt, homeAt)
	}
}

// The Putnami home of a job is the one the CLI resolves its putnami-home
// candidates against: PUTNAMI_HOME, else .putnami under the user's home
// directory, else .putnami under the workspace root.
func TestGoToolchainRootFollowsThePutnamiHome(t *testing.T) {
	ws := jobtest.RealTempDir(t)
	home := jobtest.RealTempDir(t)
	relocated := jobtest.RealTempDir(t)
	homeVariable := "HOME"
	if runtime.GOOS == "windows" {
		homeVariable = "USERPROFILE"
	}
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"PUTNAMI_HOME", []string{"PUTNAMI_HOME=" + relocated, homeVariable + "=" + home}, filepath.Join(relocated, "toolchains", "go")},
		{"the user's home directory", []string{homeVariable + "=" + home}, filepath.Join(home, ".putnami", "toolchains", "go")},
		{"no home", nil, filepath.Join(ws, ".putnami", "toolchains", "go")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, _, _ := jobtest.NewJob(t, &jobtest.Recorder{}, tc.env, ws)
			if got := j.GoToolchainRoot(); got != tc.want {
				t.Fatalf("GoToolchainRoot = %q, want %q", got, tc.want)
			}
		})
	}
}
