package jobs

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/python/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/putnamihome"
)

// stubUV is the uv program the job tests resolve: the release `putnami install`
// provisions under the Putnami home.
const stubUV = "/putnami-home/toolchains/uv/uv-0.11.32/uv"

// stubResolveUV replaces the uv the jobs resolve for a workspace.
func stubResolveUV(t *testing.T, resolve func(wsRoot string) (string, error)) {
	t.Helper()
	orig := resolveUV
	t.Cleanup(func() { resolveUV = orig })
	resolveUV = resolve
}

// uvCall is one uv command a job ran.
type uvCall struct {
	uv   string
	args []string
	dir  string
}

// uvStub records how the jobs select and run uv.
type uvStub struct {
	// resolved counts the workspaces whose uv the jobs resolved.
	resolved int
	// calls are the uv commands run.
	calls []uvCall
}

// stubUVResolution makes the jobs resolve uv 0.11.32 at stubUV, and records
// every uv command instead of running it.
func stubUVResolution(t *testing.T) *uvStub {
	t.Helper()
	stub := &uvStub{}
	origResolve, origVersion, origRun := resolveUV, uvVersion, uvCommandRun
	t.Cleanup(func() { resolveUV, uvVersion, uvCommandRun = origResolve, origVersion, origRun })
	resolveUV = func(string) (string, error) {
		stub.resolved++
		return stubUV, nil
	}
	uvVersion = func(path string) (string, error) {
		if path != stubUV {
			return "", errors.New("no uv at " + path)
		}
		return "0.11.32", nil
	}
	uvCommandRun = func(uv string, args []string, dir string, _ []string) ([]byte, error) {
		stub.calls = append(stub.calls, uvCall{uv: uv, args: append([]string(nil), args...), dir: dir})
		return nil, nil
	}
	return stub
}

// pythonWorkspace makes a workspace with the Python member pkg and the root
// pyproject.toml rootPyproject, or none when it is "".
func pythonWorkspace(t *testing.T, rootPyproject string) string {
	t.Helper()
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, ".putnamirc.json"), []byte(`{"projects": ["pkg"]}`), 0644)
	os.MkdirAll(filepath.Join(tmp, "pkg"), 0755)
	os.WriteFile(filepath.Join(tmp, "pkg/pyproject.toml"), []byte("[project]\nname = \"pkg\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"), 0644)
	if rootPyproject != "" {
		os.WriteFile(filepath.Join(tmp, "pyproject.toml"), []byte(rootPyproject), 0644)
	}
	return tmp
}

func TestWorkspaceInstall_SkipWhenNoMembers(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "workspace-scope", "a-workspace-with-no-python-members-reports-skipped")
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, ".putnamirc.json"), []byte(`{"projects": []}`), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "SKIP" {
			t.Errorf("expected SKIP, got %s", status)
		}
	})

	// Should have emitted phase start + phase end (skipped)
	if len(events) < 2 {
		t.Errorf("expected at least 2 events (phase start/end), got %d", len(events))
	}
}

func TestWorkspaceInstall_SkipWhenLockFresh(t *testing.T) {
	stub := stubUVResolution(t)
	// Pre-create root pyproject.toml so SyncUVWorkspace reports updated=false
	tmp := pythonWorkspace(t, "[tool.uv.workspace]\nmembers = [\"pkg\"]\n\n[tool.uv.sources]\npkg = { workspace = true }\n")

	// Create existing uv.lock so the skip-when-fresh path is taken
	os.WriteFile(filepath.Join(tmp, "uv.lock"), []byte("# lock"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" {
			t.Errorf("expected OK, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}
	if len(stub.calls) != 0 {
		t.Errorf("uv ran on a fresh uv.lock: %v", stub.calls)
	}
}

func TestWorkspaceInstall_ForceFlagOverridesSkip(t *testing.T) {
	ctx := &pctx.Context{
		WorkspaceRoot: t.TempDir(),
		Params:        pctx.Params{},
	}
	os.WriteFile(filepath.Join(ctx.WorkspaceRoot, ".putnamirc.json"), []byte(`{"projects": []}`), 0644)

	// With no members, it returns SKIP regardless of force
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, []string{"--force"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "SKIP" {
			t.Errorf("expected SKIP (no members), got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}
}

func TestWorkspaceInstall_NoWorkspaceConfig(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "workspace-scope", "a-missing-workspace-config-is-reported-not-invented")
	// Workspace with no config file — no members discovered → SKIP
	tmp := t.TempDir()

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "SKIP" {
			t.Errorf("expected SKIP, got %s", status)
		}
	})

	if len(events) < 2 {
		t.Errorf("expected at least 2 events (phase start/end), got %d", len(events))
	}
}

func TestWorkspaceInstall_UpdatedWorkspace(t *testing.T) {
	stub := stubUVResolution(t)
	tmp := pythonWorkspace(t, "")

	// No pre-existing root pyproject.toml, so SyncUVWorkspace will create it (updated=true)
	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" {
			t.Errorf("expected OK, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}

	// Verify the root pyproject.toml was created
	if !FileExists(filepath.Join(tmp, "pyproject.toml")) {
		t.Error("expected root pyproject.toml to be created")
	}
	if len(stub.calls) != 1 {
		t.Errorf("uv commands = %v, want one uv lock", stub.calls)
	}
}

func TestWorkspaceInstall_ForceWithMembers(t *testing.T) {
	stub := stubUVResolution(t)
	// Pre-create pyproject.toml and uv.lock (normally would skip lock)
	tmp := pythonWorkspace(t, "[tool.uv.workspace]\nmembers = [\"pkg\"]\n\n[tool.uv.sources]\npkg = { workspace = true }\n")
	os.WriteFile(filepath.Join(tmp, "uv.lock"), []byte("# lock"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		// With --force, should proceed to lock phase even when uv.lock exists
		status, _, err := WorkspaceInstall(ctx, emit, []string{"--force"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" {
			t.Errorf("expected OK, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}
	if len(stub.calls) != 1 {
		t.Errorf("uv commands = %v, want one uv lock", stub.calls)
	}
}

// workspace-install runs the uv the CLI resolved, installs none, locks with it,
// and declares its release in the root pyproject.toml, which the CLI then pins
// in putnami.lock.json.
func TestWorkspaceInstall_LocksWithTheResolvedUVAndDeclaresIt(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "pinned-uv", "workspace-install-runs-the-resolved-uv-and-declares-it")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	stub := stubUVResolution(t)
	tmp := pythonWorkspace(t, "[project]\nname = \"ws\"\n\n[tool.uv.workspace]\nmembers = [\"pkg\"]\n\n[tool.uv]\n\n[tool.uv.sources]\npkg = { workspace = true }\n")

	captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(&pctx.Context{WorkspaceRoot: tmp}, emit, []string{"--force"})
		if err != nil || status != "OK" {
			t.Fatalf("WorkspaceInstall = %s, %v; want OK", status, err)
		}
	})

	if stub.resolved != 1 {
		t.Fatalf("the job resolved uv %d times, want once", stub.resolved)
	}
	want := []uvCall{{uv: stubUV, args: []string{"lock"}, dir: tmp}}
	if !reflect.DeepEqual(stub.calls, want) {
		t.Fatalf("uv commands = %+v, want %+v", stub.calls, want)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[tool.uv]\nrequired-version = \"==0.11.32\"\n") {
		t.Fatalf("pyproject.toml declares no uv release:\n%s", data)
	}
}

// A hosted run writes nothing the repository commits: it runs the uv the
// runner holds and leaves the manifest as it is.
func TestWorkspaceInstall_HostedRunDeclaresNothing(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "pinned-uv", "a-hosted-run-declares-nothing")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	stub := stubUVResolution(t)
	manifest := "[tool.uv.workspace]\nmembers = [\"pkg\"]\n\n[tool.uv]\n\n[tool.uv.sources]\npkg = { workspace = true }\n"
	tmp := pythonWorkspace(t, manifest)

	captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(&pctx.Context{WorkspaceRoot: tmp}, emit, []string{"--force"})
		if err != nil || status != "OK" {
			t.Fatalf("WorkspaceInstall = %s, %v; want OK", status, err)
		}
	})

	if stub.resolved != 1 || len(stub.calls) != 1 {
		t.Fatalf("resolved %d times, ran %v; want the resolved uv to lock once", stub.resolved, stub.calls)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != manifest {
		t.Fatalf("a hosted run rewrote pyproject.toml:\n%s", data)
	}
}

// A required-version that names no single release, such as a range, pins
// nothing in the CLI. workspace-install then fails with a diagnostic that says
// what to declare, instead of installing a release nobody declared and
// passing, and a task that runs uv fails with the same diagnostic: no install
// could ever give it a pin. Nothing is downloaded, nothing is written, and no
// uv runs.
func TestARangeDeclarationFailsInstallAndTasksWithOneDiagnostic(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "exact-uv-declaration", "a-required-version-that-names-no-release-fails-install-and-tasks")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	t.Setenv(putnamihome.Env, t.TempDir())
	ran := 0
	origRun := uvCommandRun
	t.Cleanup(func() { uvCommandRun = origRun })
	uvCommandRun = func(string, []string, string, []string) ([]byte, error) {
		ran++
		return nil, nil
	}
	manifest := "[project]\nname = \"ws\"\n\n[tool.uv.workspace]\nmembers = [\"pkg\"]\n\n" +
		"[tool.uv]\nrequired-version = \">=0.5\"\n\n[tool.uv.sources]\npkg = { workspace = true }\n"
	tmp := pythonWorkspace(t, manifest)
	if err := os.WriteFile(filepath.Join(tmp, toolchain.WorkspaceLockFile), []byte(`{"version":4,"extensions":{},"templates":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	const diagnostic = `pyproject.toml [tool.uv] required-version is ">=0.5", which names no single uv release: ` +
		`declare one exact release, such as required-version = "==` + toolchain.DefaultUVVersion + `", so Putnami can pin it`
	carries := func(events []map[string]any) bool {
		for _, event := range events {
			if message, _ := event["message"].(string); strings.Contains(message, diagnostic) {
				return true
			}
		}
		return false
	}

	installEvents := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(&pctx.Context{WorkspaceRoot: tmp}, emit, []string{"--force"})
		if err != nil || status != "FAILED" {
			t.Fatalf("WorkspaceInstall = %s, %v; want FAILED", status, err)
		}
	})
	if !carries(installEvents) {
		t.Fatalf("workspace-install does not report %q: %v", diagnostic, installEvents)
	}

	taskEvents := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Test(&pctx.Context{
			WorkspaceRoot: tmp,
			Project:       pctx.Project{Name: "pkg", Path: "pkg", FullPath: filepath.Join(tmp, "pkg")},
		}, emit, nil)
		if err != nil || status != "FAILED" {
			t.Fatalf("Test = %s, %v; want FAILED", status, err)
		}
	})
	if !carries(taskEvents) {
		t.Fatalf("the test task does not report %q: %v", diagnostic, taskEvents)
	}

	if ran != 0 {
		t.Fatalf("uv ran %d times", ran)
	}
	if data, err := os.ReadFile(filepath.Join(tmp, "pyproject.toml")); err != nil || string(data) != manifest {
		t.Fatalf("pyproject.toml = %q, %v; want it unchanged", data, err)
	}
	if entries, err := os.ReadDir(os.Getenv(putnamihome.Env)); err != nil || len(entries) != 0 {
		t.Fatalf("the Putnami home holds %v, %v; want nothing installed", entries, err)
	}
}

// Without the uv the CLI resolves, workspace-install fails with a diagnostic
// that names the release and `putnami install`, runs no uv, installs none and
// requests nothing, even when the lock names a source to download from.
func TestWorkspaceInstall_FailsNamingPutnamiInstallWhenNoUVResolves(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "pinned-uv", "a-job-without-a-uv-fails-naming-putnami-install-and-requests-nothing")
	for _, hosted := range []string{"", "1"} {
		t.Run("hosted="+hosted, func(t *testing.T) {
			t.Setenv(extensionproto.OfflineDependenciesEnv, hosted)
			t.Setenv("PATH", t.TempDir())
			t.Setenv(putnamihome.Env, t.TempDir())
			var requests atomic.Int64
			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.NotFound(w, r)
			}))
			t.Cleanup(mirror.Close)
			ran := 0
			origRun := uvCommandRun
			t.Cleanup(func() { uvCommandRun = origRun })
			uvCommandRun = func(string, []string, string, []string) ([]byte, error) {
				ran++
				return nil, nil
			}
			tmp := pythonWorkspace(t, "")
			release := toolchain.UVRelease{
				Version:     toolchain.DefaultUVVersion,
				Integrities: map[string]string{runtime.GOOS + "/" + runtime.GOARCH: strings.Repeat("a", 64)},
				Source:      mirror.URL + "/",
			}
			lock, err := json.Marshal(map[string]any{"version": 3, "toolchains": map[string]any{"uv": release}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tmp, toolchain.WorkspaceLockFile), lock, 0o644); err != nil {
				t.Fatal(err)
			}

			events := captureEvents(t, func(emit *jsonl.Emitter) {
				status, _, err := WorkspaceInstall(&pctx.Context{WorkspaceRoot: tmp}, emit, nil)
				if err != nil || status != "FAILED" {
					t.Fatalf("WorkspaceInstall = %s, %v; want FAILED", status, err)
				}
			})

			found := false
			for _, event := range events {
				message, _ := event["message"].(string)
				if strings.Contains(message, "no uv "+toolchain.DefaultUVVersion) && strings.Contains(message, "`putnami install`") {
					found = true
				}
			}
			if !found {
				t.Fatalf("no diagnostic names uv %s and `putnami install`: %v", toolchain.DefaultUVVersion, events)
			}
			if ran != 0 || requests.Load() != 0 {
				t.Fatalf("uv ran %d times and the job sent %d requests; want none", ran, requests.Load())
			}
			if entries, err := os.ReadDir(os.Getenv(putnamihome.Env)); err != nil || len(entries) != 0 {
				t.Fatalf("the Putnami home holds %v, %v; want nothing installed", entries, err)
			}
		})
	}
}

func TestDepsUpgrade_RunsTheResolvedUV(t *testing.T) {
	stub := stubUVResolution(t)
	tmp := pythonWorkspace(t, "")

	captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := DepsUpgrade(&pctx.Context{WorkspaceRoot: tmp}, emit, nil)
		if err != nil || status != "OK" {
			t.Fatalf("DepsUpgrade = %s, %v; want OK", status, err)
		}
	})

	want := []uvCall{{uv: stubUV, args: []string{"lock", "--upgrade"}, dir: tmp}}
	if !reflect.DeepEqual(stub.calls, want) {
		t.Fatalf("uv commands = %+v, want %+v", stub.calls, want)
	}
}
