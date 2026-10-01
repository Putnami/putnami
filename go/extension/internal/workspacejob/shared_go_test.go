package workspacejob_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
)

// lockedWorkspace is a workspace that requires Go 1.99.0 and whose lock pins
// lockedGo, served by source with the SHA-256 of archive.
func lockedWorkspace(t *testing.T, source string, archive []byte) string {
	t.Helper()
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
	writeLock(t, ws, map[string]any{
		"version":     lockedGo,
		"integrities": map[string]string{hostPlatform(): digest(archive)},
		"source":      source,
	})
	return ws
}

// A second workspace that pins the release a first one installed runs the
// install in the Putnami home: it asks the download source for nothing and
// holds no Go of its own.
func TestASecondWorkspaceUsesTheInstalledGoWithoutADownload(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"a-second-workspace-downloads-nothing")
	archive := hostArchive(t, goDistribution())
	server, requests := distributionServer(t, archive, http.StatusOK)
	putnamiHome := jobtest.RealTempDir(t)

	first, rec, _ := shellFreeJobAt(t, lockedWorkspace(t, server.URL+"/dl", archive), putnamiHome)
	if !first.ResolveGoBinary() {
		t.Fatalf("ResolveGoBinary in the first workspace failed:\n%s", rec.Transcript())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("archive requests after the first workspace = %d, want 1", got)
	}
	root := first.GoToolchainRoot()
	installed := entryNames(t, root)

	ws := lockedWorkspace(t, server.URL+"/dl", archive)
	second, rec2, fakes := shellFreeJobAt(t, ws, putnamiHome)
	if !second.ResolveGoBinary() {
		t.Fatalf("ResolveGoBinary in the second workspace failed:\n%s", rec2.Transcript())
	}
	if second.GoBinary != first.GoBinary {
		t.Fatalf("the second workspace runs %q, want the install of the first, %q", second.GoBinary, first.GoBinary)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("the second workspace asked the download source: %d archive requests, want 1", got)
	}
	if got := entryNames(t, root); !slices.Equal(got, installed) {
		t.Fatalf("the second workspace changed %s: %v, was %v", root, got, installed)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".putnami")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the second workspace holds a copy of its own: %v", err)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Fatalf("the resolution started shell programs: %v", invocations)
	}
}

// Two workspaces that pin one release and install it at the same time leave
// one complete install: one job downloads the archive while the other waits
// on the install lock, and both report the same go command.
func TestInstallsThatStartTogetherLeaveOneCompleteInstall(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"installs-that-start-together-leave-one-complete-install")
	archive := hostArchive(t, goDistribution())
	putnamiHome := jobtest.RealTempDir(t)

	// The source answers once both jobs have found no install and started
	// theirs, so neither can return the other's install without waiting on
	// the lock. The wait is bounded: a job that failed before it started its
	// install fails the test through its own assertion.
	const jobs = 2
	recorders := make([]*jobtest.Recorder, jobs)
	prepared := make(chan struct{})
	bothInstalling := func() bool {
		for _, rec := range recorders {
			if !rec.Contains("Installing Go " + lockedGo + "...") {
				return false
			}
		}
		return true
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		<-prepared
		for deadline := time.Now().Add(30 * time.Second); !bothInstalling() && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)

	workspaces := make([]string, jobs)
	installers := make([]*workspacejob.Job, jobs)
	for i := range jobs {
		workspaces[i] = lockedWorkspace(t, server.URL+"/dl", archive)
		installers[i], recorders[i], _ = shellFreeJobAt(t, workspaces[i], putnamiHome)
	}
	close(prepared)

	succeeded := make([]bool, jobs)
	var wg sync.WaitGroup
	for i, installer := range installers {
		wg.Go(func() { succeeded[i] = installer.ResolveGoBinary() })
	}
	wg.Wait()

	root := installers[0].GoToolchainRoot()
	binary := workspacejob.ManagedGoBinary(root, lockedGo)
	for i, installer := range installers {
		if !succeeded[i] {
			t.Errorf("install %d failed:\n%s", i, recorders[i].Transcript())
		}
		if installer.GoBinary != binary {
			t.Errorf("install %d selected %q, want the one install %q", i, installer.GoBinary, binary)
		}
		if _, err := os.Lstat(filepath.Join(workspaces[i], ".putnami")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("install %d wrote into its workspace: %v", i, err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("archive requests = %d, want 1: one job downloads, the other waits for it", got)
	}
	if !workspacejob.GoInstallComplete(lockedGo)(workspacejob.ManagedGoDir(root, lockedGo)) {
		t.Error("the install is not complete")
	}
	// One install directory and its lock file: no second copy, no download
	// file and no staging directory is left beside them.
	if got, want := entryNames(t, root), []string{"go-" + lockedGo, "go-" + lockedGo + ".lock"}; !slices.Equal(got, want) {
		t.Errorf("%s holds %v, want %v", root, got, want)
	}
}

// A workspace that holds a Go release inside it, under the extension state
// root, keeps running that copy while the Putnami home holds none: the job
// downloads nothing and installs nothing.
func TestAGoInsideTheWorkspaceRunsWithoutADownload(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"a-go-inside-the-workspace-runs-without-a-download")
	archive := hostArchive(t, goDistribution())
	server, requests := distributionServer(t, archive, http.StatusOK)
	ws := lockedWorkspace(t, server.URL+"/dl", archive)
	putnamiHome := jobtest.RealTempDir(t)
	j, rec, fakes := shellFreeJobAt(t, ws, putnamiHome)

	copyRoot := filepath.Join(ws, ".putnami", "extensions", "@putnami-go", "libs")
	if got := j.WorkspaceGoRoot(); got != copyRoot {
		t.Fatalf("WorkspaceGoRoot = %q, want %q", got, copyRoot)
	}
	for _, file := range goDistribution() {
		path := filepath.Join(workspacejob.ManagedGoDir(copyRoot, lockedGo), filepath.FromSlash(file.name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.content), os.FileMode(file.mode)); err != nil {
			t.Fatal(err)
		}
	}
	binary := workspacejob.ManagedGoBinary(copyRoot, lockedGo)

	if !j.ResolveGoBinary() {
		t.Fatalf("ResolveGoBinary failed:\n%s", rec.Transcript())
	}
	if j.GoBinary != binary {
		t.Fatalf("GoBinary = %q, want the copy inside the workspace %q", j.GoBinary, binary)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("archive requests = %d, want none", got)
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

	// Once the Putnami home holds the release too, that install is the one
	// the job runs, as it is the first one the CLI's candidates name.
	other, rec2, _ := shellFreeJobAt(t, lockedWorkspace(t, server.URL+"/dl", archive), putnamiHome)
	if !other.ResolveGoBinary() {
		t.Fatalf("ResolveGoBinary in another workspace failed:\n%s", rec2.Transcript())
	}
	again, rec3, _ := shellFreeJobAt(t, ws, putnamiHome)
	if !again.FindGoBinary() {
		t.Fatalf("FindGoBinary found no go:\n%s", rec3.Transcript())
	}
	if want := workspacejob.ManagedGoBinary(again.GoToolchainRoot(), lockedGo); again.GoBinary != want {
		t.Fatalf("GoBinary = %q, want the install in the Putnami home %q", again.GoBinary, want)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("archive requests = %d, want the one of the other workspace", got)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Fatalf("the resolution started shell programs: %v", invocations)
	}
}

// A job limited to programs outside the workspace runs a managed Go from a
// Putnami home outside it, and none from a Putnami home that falls back to
// the workspace's own .putnami directory.
func TestAManagedGoInsideTheWorkspaceIsNotRunByWorkspaceFetch(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"workspace-fetch-runs-no-managed-go-inside-the-workspace")
	archive := hostArchive(t, goDistribution())
	place := func(t *testing.T, root string) string {
		t.Helper()
		for _, file := range goDistribution() {
			path := filepath.Join(workspacejob.ManagedGoDir(root, lockedGo), filepath.FromSlash(file.name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(file.content), os.FileMode(file.mode)); err != nil {
				t.Fatal(err)
			}
		}
		return workspacejob.ManagedGoBinary(root, lockedGo)
	}
	homeVariable := "HOME"
	if runtime.GOOS == "windows" {
		homeVariable = "USERPROFILE"
	}

	t.Run("a Putnami home outside the workspace", func(t *testing.T) {
		ws := lockedWorkspace(t, "http://127.0.0.1:1/dl", archive)
		env := []string{"PATH=" + t.TempDir(), homeVariable + "=" + jobtest.RealTempDir(t)}
		j, rec, _ := jobtest.NewJob(t, &jobtest.Recorder{}, env, ws)
		binary := place(t, j.GoToolchainRoot())
		j.OnlyProgramsOutsideTheWorkspace()
		if !j.FindGoBinary() || j.GoBinary != binary {
			t.Fatalf("FindGoBinary = %q, want the install in the Putnami home %q:\n%s", j.GoBinary, binary, rec)
		}
	})

	t.Run("a Putnami home inside the workspace", func(t *testing.T) {
		ws := lockedWorkspace(t, "http://127.0.0.1:1/dl", archive)
		env := []string{"PATH=" + t.TempDir()}
		j, _, _ := jobtest.NewJob(t, &jobtest.Recorder{}, env, ws)
		binary := place(t, j.GoToolchainRoot())
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
