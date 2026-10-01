package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// pinnedBunVersion is the release the workspaces of these tests pin. It is not
// the extension's default release, so nothing but the lock can verify it.
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
// local server, with the SHA-256 of the archive that server holds for this
// host. It returns the count of requests the server received.
func pinBunFromAMirror(t *testing.T, workspaceRoot string) *atomic.Int64 {
	t.Helper()
	arch, known := map[string]string{"amd64": "x64", "arm64": "aarch64"}[runtime.GOARCH]
	if !known || (runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows") {
		t.Skipf("Bun publishes no archive for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if loaders, _ := filepath.Glob("/lib/ld-musl-*.so.1"); len(loaders) > 0 {
		t.Skip("the lock holds no digest for the musl archive of Bun")
	}
	program := "bun"
	if runtime.GOOS == "windows" {
		program = "bun.exe"
	}
	target := "bun-" + runtime.GOOS + "-" + arch

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	header := &zip.FileHeader{Name: target + "/" + program, Method: zip.Deflate}
	header.SetMode(0o755)
	file, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("a bun program\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive.Bytes())

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/"+target+".zip" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive.Bytes())
	}))
	t.Cleanup(server.Close)

	lock := fmt.Sprintf(`{"version":3,"toolchains":{"bun":{"version":%q,"integrities":{%q:%q},"source":%q}}}`+"\n",
		pinnedBunVersion, runtime.GOOS+"/"+runtime.GOARCH, hex.EncodeToString(digest[:]), server.URL+"/")
	if err := os.WriteFile(filepath.Join(workspaceRoot, "putnami.lock.json"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	return &requests
}

// installedBun is where workspace-install puts pinnedBunVersion under home.
func installedBun(putnamiHome string) (dir, program string) {
	dir = filepath.Join(putnamiHome, "toolchains", "bun", "bun-"+pinnedBunVersion)
	program = filepath.Join(dir, "bin", "bun")
	if runtime.GOOS == "windows" {
		program += ".exe"
	}
	return dir, program
}

// On a machine with no bun, workspace-install installs the release the lock
// pins under the Putnami home and runs `bun install` with it. That bun runs
// with BUN_INSTALL and its transpiler cache under its install directory, so it
// writes nothing to .bun in the user's home directory. A second install
// downloads nothing.
func TestWorkspaceInstall_InstallsThePinnedBunAndRunsIt(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-under-the-putnami-home",
		"workspace-install-runs-the-bun-it-installed")
	home, putnamiHome := hostWithoutBun(t)
	ctx, dir := makeTestCtx(t)
	os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"fresh","private":true,"packageManager":"bun@`+pinnedBunVersion+`"}`+"\n"), 0644)
	writeExtensionBiomeDefault(t, dir)
	requests := pinBunFromAMirror(t, dir)
	installDir, program := installedBun(putnamiHome)

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

	for attempt := 1; attempt <= 2; attempt++ {
		runs = nil
		status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
		if err != nil || status != "OK" {
			t.Fatalf("install %d: runWorkspaceInstall = %q, %v; want OK", attempt, status, err)
		}
		if info, err := os.Stat(program); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("install %d: no bun at %s: %v", attempt, program, err)
		}
		if len(runs) == 0 {
			t.Fatalf("install %d: bun never ran", attempt)
		}
		last := runs[len(runs)-1]
		if last.args != "install" {
			t.Fatalf("install %d: the last bun run is %q, want `bun install`; runs = %+v", attempt, last.args, runs)
		}
		for _, run := range runs {
			if run.program != program {
				t.Errorf("install %d: `bun %s` ran %s, want the install %s", attempt, run.args, run.program, program)
			}
		}
		if last.bunInstall != installDir {
			t.Errorf("install %d: BUN_INSTALL = %q during bun install, want %q", attempt, last.bunInstall, installDir)
		}
		if want := filepath.Join(installDir, "install", "cache", "@t@"); last.transpilerCache != want {
			t.Errorf("install %d: transpiler cache = %q during bun install, want %q", attempt, last.transpilerCache, want)
		}
		if want := filepath.Join(installDir, "bin"); last.firstOnPath != want {
			t.Errorf("install %d: first PATH directory = %q during bun install, want %q", attempt, last.firstOnPath, want)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("install %d: the mirror received %d requests, want 1 in all", attempt, got)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".bun")); !os.IsNotExist(err) {
		t.Errorf("the install wrote .bun in the user's home directory: %v", err)
	}
}

// A hosted run installs no toolchain: workspace-install and workspace-fetch
// fail on a runner that holds no bun, name the release and where to put it,
// request nothing, and start no process.
func TestHostedJobs_DownloadNoBun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-under-the-putnami-home",
		"hosted-jobs-download-no-bun")
	jobs := map[string]func(t *testing.T) (string, error){
		"workspace-install": func(t *testing.T) (string, error) {
			ctx, dir := makeTestCtx(t)
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
		t.Run(name, func(t *testing.T) {
			_, putnamiHome := hostWithoutBun(t)
			t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
			mockAllExec(t, func(bin string, args []string, _ ...exec.Option) (*exec.Result, error) {
				t.Errorf("%s %v ran although the runner holds no bun", bin, args)
				return &exec.Result{Success: true}, nil
			})

			status, err := run(t)
			if status != "FAILED" || err == nil {
				t.Fatalf("%s = %q, %v; want a failure", name, status, err)
			}
			for _, want := range []string{"Bun " + pinnedBunVersion, "hosted run installs no toolchain"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not hold %q", err, want)
				}
			}
			if _, statErr := os.Lstat(filepath.Join(putnamiHome, "toolchains")); !os.IsNotExist(statErr) {
				t.Errorf("a hosted run wrote under the Putnami home: %v", statErr)
			}
		})
	}
}
