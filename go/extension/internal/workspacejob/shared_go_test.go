package workspacejob_test

import (
	"errors"
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

// The job runs the Go release the workspace lock pins from
// toolchains/go/go-<version> under the Putnami home, where the CLI provisions
// it, from its own GOROOT and ahead of any other go. It writes nothing there
// and nothing into the workspace, and starts no shell.
func TestTheJobRunsTheGoProvisionedUnderThePutnamiHome(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"the-job-runs-the-pinned-go-provisioned-under-the-putnami-home")
	ws := lockedWorkspace(t)
	putnamiHome := jobtest.RealTempDir(t)
	j, rec, fakes := shellFreeJobAt(t, ws, putnamiHome)

	root := filepath.Join(putnamiHome, "toolchains", "go")
	if got := j.GoToolchainRoot(); got != root {
		t.Fatalf("GoToolchainRoot = %q, want %q", got, root)
	}
	binary := provisionGo(t, root)
	provisioned := entryNames(t, root)

	if !j.FindGoBinary() {
		t.Fatalf("FindGoBinary found no go:\n%s", rec.Transcript())
	}
	if j.GoBinary != binary {
		t.Fatalf("GoBinary = %q, want the provisioned %q", j.GoBinary, binary)
	}

	j.SetupGoEnv("")
	goRoot := filepath.Join(workspacejob.ManagedGoDir(root, lockedGo), "go")
	if got := j.Env.Get("GOROOT"); got != goRoot {
		t.Fatalf("GOROOT = %q, want %q", got, goRoot)
	}
	if got := j.Env.Get("PATH"); !strings.HasPrefix(got, filepath.Join(goRoot, "bin")+string(filepath.ListSeparator)) {
		t.Fatalf("PATH = %q, want the provisioned go first", got)
	}

	if got := entryNames(t, root); !slices.Equal(got, provisioned) {
		t.Fatalf("the job changed %s: %v, was %v", root, got, provisioned)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".putnami")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the job wrote into the workspace: %v", err)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Fatalf("the selection started shell programs: %v", invocations)
	}
}

// Every workspace of the machine that pins one release runs the one copy the
// CLI provisioned in the Putnami home, and holds no Go of its own.
func TestASecondWorkspaceRunsTheSameProvisionedGo(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"a-second-workspace-runs-the-same-provisioned-go")
	putnamiHome := jobtest.RealTempDir(t)

	first, rec, _ := shellFreeJobAt(t, lockedWorkspace(t), putnamiHome)
	root := first.GoToolchainRoot()
	binary := provisionGo(t, root)
	if !first.FindGoBinary() || first.GoBinary != binary {
		t.Fatalf("FindGoBinary in the first workspace = %q, want %q:\n%s", first.GoBinary, binary, rec.Transcript())
	}
	provisioned := entryNames(t, root)

	ws := lockedWorkspace(t)
	second, rec2, fakes := shellFreeJobAt(t, ws, putnamiHome)
	if !second.FindGoBinary() || second.GoBinary != binary {
		t.Fatalf("FindGoBinary in the second workspace = %q, want the one copy %q:\n%s", second.GoBinary, binary, rec2.Transcript())
	}
	if got := entryNames(t, root); !slices.Equal(got, provisioned) {
		t.Fatalf("the second workspace changed %s: %v, was %v", root, got, provisioned)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".putnami")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the second workspace holds a copy of its own: %v", err)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Fatalf("the selection started shell programs: %v", invocations)
	}
}

// A workspace that holds a Go release inside it, under the extension state
// root, runs that copy while the Putnami home holds none, and the release
// provisioned in the Putnami home once it holds one.
func TestAGoInsideTheWorkspaceRunsWithoutADownload(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"a-go-inside-the-workspace-runs-without-a-download")
	ws := lockedWorkspace(t)
	putnamiHome := jobtest.RealTempDir(t)
	j, rec, fakes := shellFreeJobAt(t, ws, putnamiHome)

	copyRoot := filepath.Join(ws, ".putnami", "extensions", "@putnami-go", "libs")
	if got := j.WorkspaceGoRoot(); got != copyRoot {
		t.Fatalf("WorkspaceGoRoot = %q, want %q", got, copyRoot)
	}
	binary := provisionGo(t, copyRoot)

	if !j.FindGoBinary() {
		t.Fatalf("FindGoBinary failed:\n%s", rec.Transcript())
	}
	if j.GoBinary != binary {
		t.Fatalf("GoBinary = %q, want the copy inside the workspace %q", j.GoBinary, binary)
	}
	if got := entryNames(t, putnamiHome); len(got) != 0 {
		t.Fatalf("the Putnami home holds %v, want nothing", got)
	}

	// The copy runs from its own GOROOT, ahead of any other go.
	j.SetupGoEnv("")
	goRoot := filepath.Join(workspacejob.ManagedGoDir(copyRoot, lockedGo), "go")
	if got := j.Env.Get("GOROOT"); got != goRoot {
		t.Fatalf("GOROOT = %q, want %q", got, goRoot)
	}

	// Once the Putnami home holds the release too, that copy is the one the
	// job runs, as it is the one the CLI provisions and puts first on PATH.
	again, rec2, _ := shellFreeJobAt(t, ws, putnamiHome)
	home := provisionGo(t, again.GoToolchainRoot())
	if !again.FindGoBinary() {
		t.Fatalf("FindGoBinary found no go:\n%s", rec2.Transcript())
	}
	if again.GoBinary != home {
		t.Fatalf("GoBinary = %q, want the release in the Putnami home %q", again.GoBinary, home)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Fatalf("the selection started shell programs: %v", invocations)
	}
}

// A job limited to programs outside the workspace runs a Go provisioned in a
// Putnami home outside it, and none from a Putnami home that falls back to
// the workspace's own .putnami directory.
func TestAManagedGoInsideTheWorkspaceIsNotRunByWorkspaceFetch(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"workspace-fetch-runs-no-managed-go-inside-the-workspace")
	homeVariable := "HOME"
	if runtime.GOOS == "windows" {
		homeVariable = "USERPROFILE"
	}

	t.Run("a Putnami home outside the workspace", func(t *testing.T) {
		ws := lockedWorkspace(t)
		env := []string{"PATH=" + t.TempDir(), homeVariable + "=" + jobtest.RealTempDir(t)}
		j, rec, _ := jobtest.NewJob(t, &jobtest.Recorder{}, env, ws)
		binary := provisionGo(t, j.GoToolchainRoot())
		j.OnlyProgramsOutsideTheWorkspace()
		if !j.FindGoBinary() || j.GoBinary != binary {
			t.Fatalf("FindGoBinary = %q, want the release in the Putnami home %q:\n%s", j.GoBinary, binary, rec)
		}
	})

	t.Run("a Putnami home inside the workspace", func(t *testing.T) {
		ws := lockedWorkspace(t)
		env := []string{"PATH=" + t.TempDir()}
		j, _, _ := jobtest.NewJob(t, &jobtest.Recorder{}, env, ws)
		binary := provisionGo(t, j.GoToolchainRoot())
		if !j.FindGoBinary() || j.GoBinary != binary {
			t.Fatalf("without the restriction FindGoBinary = %q, want %q", j.GoBinary, binary)
		}

		restricted, _, _ := jobtest.NewJob(t, &jobtest.Recorder{}, env, ws)
		restricted.OnlyProgramsOutsideTheWorkspace()
		if restricted.FindGoBinary() {
			t.Fatalf("FindGoBinary selected %q inside the workspace", restricted.GoBinary)
		}
	})
}
