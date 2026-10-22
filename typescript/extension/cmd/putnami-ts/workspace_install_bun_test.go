package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// pinnedBunVersion is the release the workspaces of these tests pin. It is not
// the extension's default release.
const pinnedBunVersion = "1.9.9"

// hostWithoutBun leaves this process on a machine that holds no bun: an empty
// home directory, a Putnami home inside it, an empty PATH and no BUN_INSTALL.
// It returns the home directory and the Putnami home.
func hostWithoutBun(t *testing.T) (home, putnamiHome string) {
	t.Helper()
	home = t.TempDir()
	putnamiHome = filepath.Join(home, ".putnami")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PUTNAMI_HOME", putnamiHome)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BUN_INSTALL", "")
	t.Setenv("BUN_RUNTIME_TRANSPILER_CACHE_PATH", "")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	return home, putnamiHome
}

// pinBunFromAMirror writes a workspace lock that pins pinnedBunVersion from a
// local server, and returns the count of requests that server received: a job
// that installs no Bun sends it none.
func pinBunFromAMirror(t *testing.T, workspaceRoot string) *atomic.Int64 {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	lock := fmt.Sprintf(`{"version":3,"toolchains":{"bun":{"version":%q,"integrities":{%q:%q},"source":%q}}}`+"\n",
		pinnedBunVersion, runtime.GOOS+"/"+runtime.GOARCH, strings.Repeat("a", 64), server.URL+"/")
	if err := os.WriteFile(filepath.Join(workspaceRoot, "putnami.lock.json"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	return &requests
}

// provisionBun places pinnedBunVersion under putnamiHome as the CLI provisions
// it, the archive's bun-<target>/bun in toolchains/bun/bun-<version>, and
// returns the release directory and the program.
func provisionBun(t *testing.T, putnamiHome string) (dir, program string) {
	t.Helper()
	arch, known := map[string]string{"amd64": "x64", "arm64": "aarch64"}[runtime.GOARCH]
	if !known || (runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows") {
		t.Skipf("Bun publishes no archive for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	name := "bun"
	if runtime.GOOS == "windows" {
		name = "bun.exe"
	}
	dir = filepath.Join(putnamiHome, "toolchains", "bun", "bun-"+pinnedBunVersion)
	program = filepath.Join(dir, "bun-"+runtime.GOOS+"-"+arch, name)
	if err := os.MkdirAll(filepath.Dir(program), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("a bun program\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, program
}

// filesUnder lists the files under root, relative to it.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// workspace-install runs the bun the CLI provisioned under the Putnami home and
// put first on PATH, with BUN_INSTALL naming the release directory, as the
// task runtime's environment binding derives it. That bun keeps its caches in
// that directory and writes nothing to .bun in the user's home directory. The
// job asks the lock's source for nothing and writes nothing under the Putnami
// home.
func TestWorkspaceInstall_RunsTheProvisionedBun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-under-the-putnami-home",
		"workspace-install-runs-the-provisioned-bun")
	home, putnamiHome := hostWithoutBun(t)
	ctx, dir := makeTestCtx(t)
	os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"fresh","private":true,"packageManager":"bun@`+pinnedBunVersion+`"}`+"\n"), 0644)
	writeExtensionBiomeDefault(t, dir)
	requests := pinBunFromAMirror(t, dir)
	installDir, program := provisionBun(t, putnamiHome)
	provisioned := filesUnder(t, putnamiHome)

	// The CLI's environment for a job of the task runtime, then the job's own
	// start.
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", dir)
	t.Setenv("BUN_INSTALL", filepath.Dir(filepath.Dir(program)))
	t.Setenv("PATH", filepath.Dir(program)+string(os.PathListSeparator)+os.Getenv("PATH"))
	toolchain.ApplyManagedBunEnv()

	type bunRun struct {
		program, args, bunInstall, transpilerCache, firstOnPath string
	}
	var runs []bunRun
	mockAllExec(t, func(bin string, args []string, _ ...exec.Option) (*exec.Result, error) {
		first, _, _ := strings.Cut(os.Getenv("PATH"), string(os.PathListSeparator))
		runs = append(runs, bunRun{
			program:         bin,
			args:            strings.Join(args, " "),
			bunInstall:      os.Getenv("BUN_INSTALL"),
			transpilerCache: os.Getenv("BUN_RUNTIME_TRANSPILER_CACHE_PATH"),
			firstOnPath:     first,
		})
		if len(args) == 1 && args[0] == "--version" {
			return &exec.Result{Success: true, Stdout: pinnedBunVersion + "\n"}, nil
		}
		return &exec.Result{Success: true}, nil
	})

	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	if len(runs) == 0 {
		t.Fatal("bun never ran")
	}
	last := runs[len(runs)-1]
	if last.args != "install" {
		t.Fatalf("the last bun run is %q, want `bun install`; runs = %+v", last.args, runs)
	}
	for _, run := range runs {
		if run.program != program {
			t.Errorf("`bun %s` ran %s, want the provisioned %s", run.args, run.program, program)
		}
	}
	if last.bunInstall != installDir {
		t.Errorf("BUN_INSTALL = %q during bun install, want %q", last.bunInstall, installDir)
	}
	if want := filepath.Join(installDir, "install", "cache", "@t@"); last.transpilerCache != want {
		t.Errorf("transpiler cache = %q during bun install, want %q", last.transpilerCache, want)
	}
	if want := filepath.Dir(program); last.firstOnPath != want {
		t.Errorf("first PATH directory = %q during bun install, want %q", last.firstOnPath, want)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("the lock's source received %d requests, want none", got)
	}
	if got := filesUnder(t, putnamiHome); !slices.Equal(got, provisioned) {
		t.Errorf("the Putnami home holds %v, want only the provisioned %v", got, provisioned)
	}
	if _, err := os.Lstat(filepath.Join(home, ".bun")); !os.IsNotExist(err) {
		t.Errorf("the install wrote .bun in the user's home directory: %v", err)
	}
}

// workspace-install and workspace-fetch install no Bun: on a machine that
// holds none, on a hosted run or not, they fail with an error that names the
// pinned release and `putnami install`, request nothing, start no process, and
// write nothing under the Putnami home.
func TestWorkspaceJobs_InstallNoBun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-under-the-putnami-home",
		"a-job-without-a-bun-fails-naming-putnami-install-and-downloads-nothing")
	jobs := map[string]func(t *testing.T) (string, error){
		"workspace-install": func(t *testing.T) (string, error) {
			ctx, dir := makeTestCtx(t)
			t.Setenv("PUTNAMI_WORKSPACE_ROOT", dir)
			os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"ws","private":true}`), 0644)
			requests := pinBunFromAMirror(t, dir)
			t.Cleanup(func() {
				if got := requests.Load(); got != 0 {
					t.Errorf("the mirror received %d requests, want none", got)
				}
			})
			status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
			return status, err
		},
		"workspace-fetch": func(t *testing.T) (string, error) {
			ctx := fetchUnitWorkspace(t)
			t.Setenv("PUTNAMI_WORKSPACE_ROOT", ctx.WorkspaceRoot)
			requests := pinBunFromAMirror(t, ctx.WorkspaceRoot)
			t.Cleanup(func() {
				if got := requests.Load(); got != 0 {
					t.Errorf("the mirror received %d requests, want none", got)
				}
			})
			var events []string
			handCredential(t, nil, &events)
			status, _, err := runWorkspaceFetch(ctx, jsonl.New(), nil)
			return status, err
		},
	}
	for name, run := range jobs {
		for _, hosted := range []string{"", "1"} {
			t.Run(name+" hosted="+hosted, func(t *testing.T) {
				_, putnamiHome := hostWithoutBun(t)
				t.Setenv(extensionproto.OfflineDependenciesEnv, hosted)
				mockAllExec(t, func(bin string, args []string, _ ...exec.Option) (*exec.Result, error) {
					t.Errorf("%s %v ran although the machine holds no bun", bin, args)
					return &exec.Result{Success: true}, nil
				})

				status, err := run(t)
				if status != "FAILED" || err == nil {
					t.Fatalf("%s = %q, %v; want a failure", name, status, err)
				}
				for _, want := range []string{"Bun " + pinnedBunVersion, "`putnami install`"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not hold %q", err, want)
					}
				}
				if _, statErr := os.Lstat(filepath.Join(putnamiHome, "toolchains")); !os.IsNotExist(statErr) {
					t.Errorf("the job wrote under the Putnami home: %v", statErr)
				}
			})
		}
	}
}
