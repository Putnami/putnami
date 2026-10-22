package workspacejob_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
)

// The CLI resolves the Go every task runs with from the manifest's
// runtimeCompiler candidates, before any task starts. The release the CLI
// provisions for `putnami install` must be one of them: the provisioned
// candidate names the executable the toolchain-resolve answer pins, unpacked
// in toolchains/go/go-<version> under the Putnami home, which is the go
// command the job selects there. No putnami-home candidate names a release
// the extension installs. A later environment candidate below
// PUTNAMI_WORKSPACE_ROOT, which the CLI sets to the workspace root, names the
// go command of a release inside the workspace, so the provisioned release
// wins when a workspace holds both.
func TestTheProvisionedGoIsARuntimeCompilerCandidate(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"the-provisioned-go-is-a-runtime-compiler-candidate")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Runtime struct {
			Toolchains map[string]struct {
				Lock       string `json:"lock"`
				Candidates []struct {
					From        string `json:"from"`
					Environment string `json:"environment"`
					Path        string `json:"path"`
				} `json:"candidates"`
			} `json:"toolchains"`
			Resolves []string `json:"resolves"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	compiler := manifest.Runtime.Toolchains["runtimeCompiler"]
	if compiler.Lock != workspacejob.GoLockIdentity || !slices.Contains(manifest.Runtime.Resolves, workspacejob.GoLockIdentity) {
		t.Fatalf("runtimeCompiler locks %q and the extension resolves %v, want both %q",
			compiler.Lock, manifest.Runtime.Resolves, workspacejob.GoLockIdentity)
	}

	ws := jobtest.RealTempDir(t)
	putnamiHome := jobtest.RealTempDir(t)
	j, _, _ := shellFreeJobAt(t, ws, putnamiHome)

	// The CLI unpacks a provisioned toolchain into
	// toolchains/<lock>/<lock>-<version> under the Putnami home and runs the
	// executable the pin names for the host platform there.
	pin := workspacejob.GoArchivesPin(workspacejob.GoLock{
		Version:     lockedGo,
		Integrities: map[string]string{hostPlatform(): strings.Repeat("a", 64)},
	})
	archive, ok := pin.Archives[hostPlatform()]
	if !ok {
		t.Fatalf("the pin names no archive for %s: %+v", hostPlatform(), pin.Archives)
	}
	provisioned := filepath.Join(putnamiHome, "toolchains", compiler.Lock, compiler.Lock+"-"+lockedGo, filepath.FromSlash(archive.Executable))
	if want := workspacejob.ManagedGoBinary(j.GoToolchainRoot(), lockedGo); provisioned != want {
		t.Fatalf("the CLI provisions go at %s, the job looks for it at %s", provisioned, want)
	}
	workspaceCopy := workspacejob.ManagedGoBinary(j.WorkspaceGoRoot(), lockedGo)

	// The CLI's expansion of an environment candidate: {version} is the locked
	// version, the path is below the variable's value, and a path without an
	// extension gains .exe on Windows.
	provisionedAt, workspaceAt := -1, -1
	for index, candidate := range compiler.Candidates {
		switch {
		case candidate.From == "putnami-home":
			t.Fatalf("candidate %d is a putnami-home candidate %q: the extension installs no Go", index, candidate.Path)
		case candidate.From == "provisioned":
			if provisionedAt >= 0 {
				t.Fatalf("candidates %d and %d are both provisioned", provisionedAt, index)
			}
			if candidate.Path != "" || candidate.Environment != "" {
				t.Fatalf("the provisioned candidate names a path or a variable: %+v", candidate)
			}
			provisionedAt = index
		case candidate.From == "environment" && candidate.Environment == "PUTNAMI_WORKSPACE_ROOT":
			got := filepath.Join(ws, filepath.FromSlash(strings.ReplaceAll(candidate.Path, "{version}", lockedGo)))
			if runtime.GOOS == "windows" && filepath.Ext(got) == "" {
				got += ".exe"
			}
			if got == workspaceCopy {
				workspaceAt = index
			}
		}
	}
	if provisionedAt < 0 {
		t.Fatal("no runtimeCompiler candidate is the provisioned Go")
	}
	if workspaceAt < 0 {
		t.Fatalf("no runtimeCompiler candidate names the release inside the workspace %s", workspaceCopy)
	}
	if provisionedAt > workspaceAt {
		t.Fatalf("the workspace candidate (%d) comes before the provisioned candidate (%d)", workspaceAt, provisionedAt)
	}
}

// The Putnami home of a job is the one the CLI provisions toolchains in: PUTNAMI_HOME, else .putnami under the user's home
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
