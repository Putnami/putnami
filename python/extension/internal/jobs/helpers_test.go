package jobs

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/python/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/hostenv"
	"go.putnami.dev/sdk/extension/jsonl"
)

// captureEvents captures JSONL events emitted to stdout during fn.
func captureEvents(t *testing.T, fn func(emit *jsonl.Emitter)) []map[string]any {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	origStdout := os.Stdout
	os.Stdout = w

	emit := jsonl.New()
	fn(emit)

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err == nil {
			events = append(events, event)
		}
	}
	return events
}

// ---- MakeEnv ----

func TestMakeEnv(t *testing.T) {
	t.Run("includes standard variables", func(t *testing.T) {
		env := MakeEnv("/workspace", "/workspace/pkg", nil)
		found := map[string]bool{}
		for _, e := range env {
			if e == "FORCE_COLOR=1" {
				found["FORCE_COLOR"] = true
			}
			if e == "PUTNAMI_WORKSPACE=/workspace" {
				found["PUTNAMI_WORKSPACE"] = true
			}
			if e == "PUTNAMI_WORKING_DIR=/workspace/pkg" {
				found["PUTNAMI_WORKING_DIR"] = true
			}
		}
		for _, key := range []string{"FORCE_COLOR", "PUTNAMI_WORKSPACE", "PUTNAMI_WORKING_DIR"} {
			if !found[key] {
				t.Errorf("expected %s in env", key)
			}
		}
	})

	t.Run("appends extra variables", func(t *testing.T) {
		env := MakeEnv("/ws", "/ws/pkg", map[string]string{
			"PORT":       "8080",
			"PYTHONPATH": "/lib",
		})
		found := map[string]bool{}
		for _, e := range env {
			if e == "PORT=8080" {
				found["PORT"] = true
			}
			if e == "PYTHONPATH=/lib" {
				found["PYTHONPATH"] = true
			}
		}
		if !found["PORT"] || !found["PYTHONPATH"] {
			t.Errorf("expected extra vars in env, found: %v", found)
		}
	})

	t.Run("nil extra is safe", func(t *testing.T) {
		env := MakeEnv("/ws", "/ws/pkg", nil)
		if len(env) == 0 {
			t.Error("expected non-empty env")
		}
	})
}

// ---- MakeTestEnv: host platform identity scrub ----

func envValue(env []string, name string) (string, bool) {
	value, found := "", false
	// Last entry wins, matching how a process resolves a duplicated variable.
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, name+"="); ok {
			value, found = v, true
		}
	}
	return value, found
}

// A pytest process must not inherit the HARNESS HOST's platform identity. On a
// CI worker that is itself a Cloud Run service the host exports K_SERVICE;
// application code reads it as the production signal and refuses test-only
// behavior, failing deterministically there and nowhere else.
func TestMakeTestEnv_ScrubsHostPlatformIdentity(t *testing.T) {
	t.Setenv("K_SERVICE", "ci-worker")
	t.Setenv("K_REVISION", "ci-worker-00042-abc")
	t.Setenv("K_CONFIGURATION", "ci-worker")
	t.Setenv("GAE_ENV", "standard")
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "fn")

	env := MakeTestEnv("/ws", "/ws/pkg", map[string]string{"PYTHONPATH": "/ws/pkg"})

	for _, name := range []string{"K_SERVICE", "K_REVISION", "K_CONFIGURATION", "GAE_ENV", "AWS_LAMBDA_FUNCTION_NAME"} {
		if value, found := envValue(env, name); found {
			t.Errorf("%s leaked into the pytest environment as %q", name, value)
		}
	}
}

// On a hosted run the engine describes the job to the extension in two
// variables. pytest must see neither: a repository's tests are not jobs of the
// run.
func TestMakeTestEnv_ScrubsTheJobVariablesOfAHostedRun(t *testing.T) {
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	t.Setenv("PUTNAMI_JOB_CREDENTIAL_FD", "7")

	env := MakeTestEnv("/ws", "/ws/pkg", nil)

	for _, name := range []string{"PUTNAMI_OFFLINE_DEPENDENCIES", "PUTNAMI_JOB_CREDENTIAL_FD"} {
		if value, found := envValue(env, name); found {
			t.Errorf("%s leaked into the pytest environment as %q", name, value)
		}
	}
}

// The scrub must cost nothing else: the harness wiring, the caller's extras,
// credentials and the capability-bearing GCP project selector all survive.
func TestMakeTestEnv_PreservesEverythingElse(t *testing.T) {
	t.Setenv("K_SERVICE", "ci-worker")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/secrets/adc.json")
	t.Setenv("DATABASE_TEST_BINDINGS", `{"protocolVersion":1,"databases":{}}`)

	env := MakeTestEnv("/ws", "/ws/pkg", map[string]string{"PYTHONPATH": "/ws/pkg:/lib"})

	for _, want := range [][2]string{
		{"FORCE_COLOR", "1"},
		{"PUTNAMI_WORKSPACE", "/ws"},
		{"PUTNAMI_WORKING_DIR", "/ws/pkg"},
		{"PYTHONPATH", "/ws/pkg:/lib"},
		{"GOOGLE_CLOUD_PROJECT", "my-project"},
		{"GOOGLE_APPLICATION_CREDENTIALS", "/secrets/adc.json"},
		{"DATABASE_TEST_BINDINGS", `{"protocolVersion":1,"databases":{}}`},
	} {
		if got, found := envValue(env, want[0]); !found || got != want[1] {
			t.Errorf("%s = %q (found=%v), want %q", want[0], got, found, want[1])
		}
	}
	if _, found := envValue(env, "PATH"); !found {
		t.Error("inherited PATH did not survive the scrub")
	}
}

// A host with no platform identity — every developer machine — must get exactly
// what MakeEnv gives.
func TestMakeTestEnv_IsNoopWithoutPlatformIdentity(t *testing.T) {
	for _, name := range hostenv.PlatformIdentityVars() {
		if _, ok := os.LookupEnv(name); ok {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}

	plain := MakeEnv("/ws", "/ws/pkg", map[string]string{"PYTHONPATH": "/ws/pkg"})
	scrubbed := MakeTestEnv("/ws", "/ws/pkg", map[string]string{"PYTHONPATH": "/ws/pkg"})
	if len(plain) != len(scrubbed) {
		t.Fatalf("MakeTestEnv dropped %d entries from an environment carrying no platform identity", len(plain)-len(scrubbed))
	}
}

// The scrub is deliberately confined to the test paths: `run` and `serve` start
// a real application, and a locally served app is a deployment that has every
// right to see where it is running.
func TestMakeEnv_KeepsHostPlatformIdentity(t *testing.T) {
	t.Setenv("K_SERVICE", "ci-worker")

	env := MakeEnv("/ws", "/ws/pkg", nil)
	if got, found := envValue(env, "K_SERVICE"); !found || got != "ci-worker" {
		t.Errorf("K_SERVICE = %q (found=%v), want it preserved for non-test subprocesses", got, found)
	}
}

// ---- uv directories stay out of HOME ----

// clearUVEnv removes every variable that could move uv's directories, so a test
// sees uv's HOME-based defaults unless the extension sets its own.
func clearUVEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{toolchain.UVPythonInstallDirEnv, toolchain.UVCacheDirEnv, "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Every uv subprocess gets workspace-local managed-Python and cache directories.
// The Putnami CI runner runs tasks as a user for whom HOME is read-only, and
// uv's defaults (~/.local/share/uv/python, ~/.cache/uv) failed every Python
// lint and test task there.
func TestMakeEnv_PointsUVAtWorkspace(t *testing.T) {
	clearUVEnv(t)
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	state := toolchain.UVStateDir(ws)
	want := map[string]string{
		toolchain.UVPythonInstallDirEnv: filepath.Join(state, "python"),
		toolchain.UVCacheDirEnv:         filepath.Join(state, "cache"),
	}

	lookups := map[string]func(string) (string, bool){
		"MakeEnv":      func(k string) (string, bool) { return envValue(MakeEnv(ws, ws, nil), k) },
		"MakeTestEnv":  func(k string) (string, bool) { return envValue(MakeTestEnv(ws, ws, nil), k) },
		"batchRuffEnv": func(k string) (string, bool) { v, ok := batchRuffEnv(ws)[k]; return v, ok },
	}
	for name, lookup := range lookups {
		for key, dir := range want {
			if got, found := lookup(key); !found || got != dir {
				t.Errorf("%s: %s = %q (found=%v), want %q", name, key, got, found, dir)
			}
		}
	}
}

// An explicit uv setting is the user's choice and wins over the default.
func TestMakeEnv_KeepsExplicitUVDirs(t *testing.T) {
	t.Setenv(toolchain.UVPythonInstallDirEnv, "/opt/uv/python")
	t.Setenv(toolchain.UVCacheDirEnv, "/opt/uv/cache")
	ws := t.TempDir()

	for name, env := range map[string][]string{
		"MakeEnv":     MakeEnv(ws, ws, nil),
		"MakeTestEnv": MakeTestEnv(ws, ws, nil),
	} {
		if got, _ := envValue(env, toolchain.UVPythonInstallDirEnv); got != "/opt/uv/python" {
			t.Errorf("%s: %s = %q, want the explicit /opt/uv/python", name, toolchain.UVPythonInstallDirEnv, got)
		}
		if got, _ := envValue(env, toolchain.UVCacheDirEnv); got != "/opt/uv/cache" {
			t.Errorf("%s: %s = %q, want the explicit /opt/uv/cache", name, toolchain.UVCacheDirEnv, got)
		}
	}
	// exec.Env layers batchRuffEnv onto the inherited environment, so the
	// explicit values reach uv as long as the map does not replace them.
	env := batchRuffEnv(ws)
	for _, key := range []string{toolchain.UVPythonInstallDirEnv, toolchain.UVCacheDirEnv} {
		if got, found := env[key]; found {
			t.Errorf("batchRuffEnv replaced the explicit %s with %q", key, got)
		}
	}
}

// The real uv, under a read-only HOME, must keep working with the environment
// the jobs build. This also pins the variable names against the uv the host
// runs: a misspelled name leaves uv on its HOME defaults and fails here.
func TestMakeEnv_UVUnderReadOnlyHome(t *testing.T) {
	uv, err := osexec.LookPath("uv")
	if err != nil {
		t.Skip("uv is not installed")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so HOME cannot be made read-only")
	}
	if runtime.GOOS == "windows" {
		// os.Chmod only sets a read-only attribute, which does not stop writes
		// into a Windows directory, and uv's Windows defaults live under
		// %LOCALAPPDATA%, not HOME.
		t.Skip("Windows cannot make HOME read-only, and uv does not default to HOME there")
	}
	clearUVEnv(t)

	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	t.Setenv("HOME", home)
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := toolchain.UVStateDir(ws)

	// Offline and managed-only, like a runner with no system Python: uv
	// looks for a managed Python and needs its cache, but downloads nothing.
	runUV := func(env []string, args ...string) (string, error) {
		cmd := osexec.Command(uv, append([]string{"--no-config"}, args...)...)
		cmd.Dir = ws
		cmdEnv := append([]string{}, env...)
		cmdEnv = append(cmdEnv, "UV_PYTHON_PREFERENCE=only-managed", "UV_PYTHON_DOWNLOADS=never", "NO_COLOR=1")
		cmd.Env = cmdEnv
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// Control: without the extension's variables, uv writes under HOME and
	// fails. If it does not, HOME is not read-only and the test proves nothing.
	if out, err := runUV(os.Environ(), "python", "find"); err == nil || !strings.Contains(out, home) {
		t.Fatalf("control: uv with its defaults did not fail on the read-only HOME (err=%v):\n%s", err, out)
	}

	env := MakeEnv(ws, ws, nil)
	for _, probe := range []struct {
		args []string
		want string
	}{
		{[]string{"cache", "dir"}, filepath.Join(state, "cache")},
		{[]string{"python", "dir"}, filepath.Join(state, "python")},
	} {
		out, err := runUV(env, probe.args...)
		if err != nil || out != probe.want {
			t.Errorf("uv %s = %q (err=%v), want %q", strings.Join(probe.args, " "), out, err, probe.want)
		}
	}

	// No managed Python exists, so find fails; it must fail for that reason,
	// not because it tried to write under HOME.
	out, _ := runUV(env, "python", "find")
	if strings.Contains(out, home) || strings.Contains(out, "Permission denied") {
		t.Errorf("uv python find still reached for HOME:\n%s", out)
	}
	if !FileExists(filepath.Join(state, "cache")) {
		t.Errorf("uv did not create its cache under the workspace at %s", filepath.Join(state, "cache"))
	}
}

// ---- FileExists ----

func TestFileExists(t *testing.T) {
	t.Run("existing file returns true", func(t *testing.T) {
		tmp := t.TempDir()
		path := filepath.Join(tmp, "file.txt")
		os.WriteFile(path, []byte("test"), 0644)
		if !FileExists(path) {
			t.Error("expected FileExists to return true for existing file")
		}
	})

	t.Run("missing file returns false", func(t *testing.T) {
		if FileExists("/nonexistent/path/file.txt") {
			t.Error("expected FileExists to return false for missing file")
		}
	})

	t.Run("existing directory returns true", func(t *testing.T) {
		tmp := t.TempDir()
		if !FileExists(tmp) {
			t.Error("expected FileExists to return true for existing directory")
		}
	})
}

// ---- ResolvePackageName ----

func TestResolvePackageName(t *testing.T) {
	t.Run("reads name from pyproject.toml", func(t *testing.T) {
		tmp := t.TempDir()
		os.WriteFile(filepath.Join(tmp, "pyproject.toml"), []byte("[project]\nname = \"my-python-pkg\"\n"), 0644)
		ctx := &pctx.Context{
			Project: pctx.Project{
				Name:     "fallback-name",
				FullPath: tmp,
			},
		}
		got := ResolvePackageName(ctx)
		if got != "my-python-pkg" {
			t.Errorf("expected my-python-pkg, got %s", got)
		}
	})

	t.Run("falls back to project name when no pyproject.toml", func(t *testing.T) {
		tmp := t.TempDir()
		ctx := &pctx.Context{
			Project: pctx.Project{
				Name:     "fallback-name",
				FullPath: tmp,
			},
		}
		got := ResolvePackageName(ctx)
		if got != "fallback-name" {
			t.Errorf("expected fallback-name, got %s", got)
		}
	})

	t.Run("falls back when pyproject.toml has no project name", func(t *testing.T) {
		tmp := t.TempDir()
		os.WriteFile(filepath.Join(tmp, "pyproject.toml"), []byte("[tool.ruff]\nline-length = 80\n"), 0644)
		ctx := &pctx.Context{
			Project: pctx.Project{
				Name:     "ctx-name",
				FullPath: tmp,
			},
		}
		got := ResolvePackageName(ctx)
		if got != "ctx-name" {
			t.Errorf("expected ctx-name, got %s", got)
		}
	})
}

// ---- SyncWorkspace ----

func TestSyncWorkspace_NoProjects(t *testing.T) {
	tmp := t.TempDir()
	// Create empty putnami.workspace.json
	os.WriteFile(filepath.Join(tmp, "putnami.workspace.json"), []byte(`{"projects": []}`), 0644)

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		// UV might not be on PATH in some envs; that's fine
		SyncWorkspace(tmp, emit)
	})

	// Should have emitted at least one event (phase start)
	if len(events) == 0 {
		t.Error("expected at least one JSONL event from SyncWorkspace")
	}
}

func TestSyncWorkspace_WithProjects(t *testing.T) {
	tmp := t.TempDir()
	// Create workspace with a Python project
	os.WriteFile(filepath.Join(tmp, "putnami.workspace.json"), []byte(`{"projects": ["packages/mypkg"]}`), 0644)
	os.MkdirAll(filepath.Join(tmp, "packages/mypkg"), 0755)
	os.WriteFile(filepath.Join(tmp, "packages/mypkg/pyproject.toml"), []byte("[project]\nname = \"mypkg\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"), 0644)

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		SyncWorkspace(tmp, emit)
	})

	if len(events) == 0 {
		t.Error("expected JSONL events from SyncWorkspace")
	}

	// Verify root pyproject.toml was created
	if !FileExists(filepath.Join(tmp, "pyproject.toml")) {
		t.Error("expected root pyproject.toml to be created")
	}
}

func TestSyncWorkspace_EmptyWorkspace(t *testing.T) {
	// Non-existent directory has no workspace config → no members → skipped phase
	tmp := t.TempDir()
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		// No workspace config → 0 members → skipped, then UV resolution
		_ = SyncWorkspace(tmp, emit)
	})

	// Should emit at least phase start + phase end (skipped)
	if len(events) < 2 {
		t.Errorf("expected at least 2 events, got %d", len(events))
	}
}

func TestSyncWorkspace_ReturnsTrue(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "putnami.workspace.json"), []byte(`{"projects": ["pkg"]}`), 0644)
	os.MkdirAll(filepath.Join(tmp, "pkg"), 0755)
	os.WriteFile(filepath.Join(tmp, "pkg/pyproject.toml"), []byte("[project]\nname = \"pkg\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"), 0644)

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		ok := SyncWorkspace(tmp, emit)
		// With uv on PATH, this should return true
		if !ok {
			t.Skip("uv not available on PATH")
		}
	})

	if len(events) == 0 {
		t.Error("expected JSONL events")
	}
}

// ---- snapshotsEqual ----

func TestSnapshotsEqual(t *testing.T) {
	tests := []struct {
		name string
		a    map[string]int64
		b    map[string]int64
		want bool
	}{
		{
			name: "both empty",
			a:    map[string]int64{},
			b:    map[string]int64{},
			want: true,
		},
		{
			name: "equal maps",
			a:    map[string]int64{"a.py": 100, "b.py": 200},
			b:    map[string]int64{"a.py": 100, "b.py": 200},
			want: true,
		},
		{
			name: "different values",
			a:    map[string]int64{"a.py": 100},
			b:    map[string]int64{"a.py": 200},
			want: false,
		},
		{
			name: "different keys",
			a:    map[string]int64{"a.py": 100},
			b:    map[string]int64{"b.py": 100},
			want: false,
		},
		{
			name: "different lengths",
			a:    map[string]int64{"a.py": 100},
			b:    map[string]int64{"a.py": 100, "b.py": 200},
			want: false,
		},
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snapshotsEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("snapshotsEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
