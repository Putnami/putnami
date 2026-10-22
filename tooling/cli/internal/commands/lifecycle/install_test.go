package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchainpins"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// installTestEnv is the LifecycleEnv the Install tests run with: every human
// line is captured on the supplied writer instead of the process stdout (which
// is the point of the writer seam, slice A5a), and the workspace-install pass
// is answered by a stub because these tests exercise Install's SEQUENCE, not
// the engine. outcome selects what the stub reports.
func installTestEnv(out *strings.Builder, outcome WorkspaceJobOutcome) LifecycleEnv {
	return LifecycleEnv{
		Out: out,
		RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
			return WorkspaceJobResult{Outcome: outcome}, nil
		},
	}
}

// noInstallProviderLine is the one line an explicit command prints when no
// loaded extension provides the "workspace-install" job.
const noInstallProviderLine = "No extension provides workspace-install, so there is nothing to install."

// TestInstall_EmptyWorkspaceFlow exercises the primary ordering of Install on a
// workspace with no extensions/templates: extensions install (no-op success),
// AI context generation, the templates phase is skipped, and finally deps
// install runs. With no extension providing the "workspace-install" job, there
// is nothing to install: Install succeeds and says so once.
func TestInstall_EmptyWorkspaceFlow(t *testing.T) {
	dir := t.TempDir()
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name": "install-ws",
	})
	// Avoid touching the developer's real home for any AI-context writes.
	hometest.Temp(t)

	var buf strings.Builder
	testEnv := installTestEnv(&buf, WorkspaceJobMissing)
	testEnv.Display.Verbose = true
	leaked, err := captureStdout(t, func() error {
		return Install(context.Background(), dir, wsproto.Load(dir), nil, testEnv)
	})
	if err != nil {
		t.Fatalf("Install with no workspace-install provider = %v, want success", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Installing extensions...") {
		t.Errorf("output = %q, want extensions phase", out)
	}
	if !strings.Contains(out, "Running workspace installers...") {
		t.Errorf("output = %q, want deps phase", out)
	}
	// No templates configured → templates phase must be skipped.
	if strings.Contains(out, "Installing templates...") {
		t.Errorf("output = %q, should not run templates phase", out)
	}
	if got := strings.Count(out, noInstallProviderLine); got != 1 {
		t.Errorf("output = %q, want the no-provider line once, got %d", out, got)
	}
	// Every human line went to the supplied writer, not the process stdout —
	// which is what lets the first-use bootstrap install under --output=json
	// without the global os.Stdout swap slice A5a deleted.
	if leaked != "" {
		t.Errorf("Install wrote to the process stdout despite LifecycleEnv.Out: %q", leaked)
	}
}

func TestInstall_ExtensionsFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	// A configured remote extension with an unreachable registry forces
	// ExtensionsInstall to fail, which Install wraps and returns.
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name": "install-ws",
		"extensions": map[string]any{
			"@putnami/go": "1.0.0",
		},
	})
	hometest.Temp(t)
	t.Setenv("PUTNAMI_REGISTRY_URL", "http://127.0.0.1:1")
	// Dead registry refuses instantly; skip the retry backoff so the test
	// doesn't pay the full (1s+2s) budget waiting on a host that won't recover.
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")

	var buf strings.Builder
	testEnv := installTestEnv(&buf, WorkspaceJobOK)
	testEnv.Display.Verbose = true
	_, err := captureStdout(t, func() error {
		return Install(context.Background(), dir, wsproto.Load(dir), nil, testEnv)
	})
	if err == nil || !strings.Contains(err.Error(), "extensions install") {
		t.Fatalf("err = %v, want extensions install failure", err)
	}
}

func TestInstall_DefaultPrintsOnlyCompletedActions(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(&wsproto.Config{Name: "install-ws"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), data, 0o644); err != nil {
		t.Fatal(err)
	}
	hometest.Temp(t)

	var out strings.Builder
	env := LifecycleEnv{
		Out:     &out,
		Display: LifecycleDisplay{Interactive: true},
		RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
			req.OnAction(LifecycleAction{
				Kind: "workspace-install", Name: "fixture",
				Description: "Workspace setup completed (fixture)",
			})
			return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
		},
	}
	if err := Install(context.Background(), dir, wsproto.Load(dir), nil, env); err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := "  ✓ Putnami MCP server registered in .mcp.json\n" +
		"  ✓ Workspace setup completed (fixture)\n" +
		"  ✓ Workspace lock metadata refreshed\n"
	if got := out.String(); got != want {
		t.Fatalf("redirected compact install output = %q, want %q", got, want)
	}
}

// An explicit `putnami install` registers the putnami MCP server, so a new
// agent session in the workspace reaches it with no manual step (ADR 0040).
// The first-use bootstrap runs the same Install on behalf of an unrelated
// command, and writes no .mcp.json.
func TestInstall_RegistersTheMCPServerOnlyWhenExplicit(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "implicit-mcp-registration", "install-registers-and-the-first-use-bootstrap-does-not")
	hometest.Temp(t)
	run := func(ctx context.Context) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(`{"name":"install-ws"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if _, err := captureStdout(t, func() error {
			return Install(ctx, dir, wsproto.Load(dir), nil, installTestEnv(&out, WorkspaceJobOK))
		}); err != nil {
			t.Fatalf("Install: %v", err)
		}
		return dir
	}

	explicit := run(context.Background())
	data, err := os.ReadFile(filepath.Join(explicit, ".mcp.json"))
	if err != nil {
		t.Fatalf("explicit install wrote no .mcp.json: %v", err)
	}
	var registration struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &registration); err != nil {
		t.Fatalf("parse .mcp.json: %v", err)
	}
	if entry, ok := registration.MCPServers["putnami"]; !ok || entry.Command != "putnami" || strings.Join(entry.Args, " ") != "mcp" {
		t.Fatalf(".mcp.json does not register the putnami server:\n%s", data)
	}

	implicit := run(shared.WithImplicitInstall(context.Background()))
	if _, err := os.Stat(filepath.Join(implicit, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("the first-use bootstrap wrote .mcp.json: stat err %v", err)
	}
}

func TestInstall_ContextGenerationFailurePropagates(t *testing.T) {
	const (
		name    = "@putnami/test"
		version = "1.0.0"
		digest  = "1111111111111111111111111111111111111111111111111111111111111111"
	)

	dir := t.TempDir()
	storeRoot := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)
	workspaceConfig := `{"name":"install-ws","extensions":{"` + name + `":"latest"}}`
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(workspaceConfig), 0o644); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}

	extDir := sharedtest.WriteContextTestExtension(t, storeRoot, digest, name, version, "")
	manifestHash, err := lockfile.HashFile(filepath.Join(extDir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("hash extension manifest: %v", err)
	}
	lf := lockfile.NewLockFile()
	lf.SetExtension(name, lockfile.LockEntry{
		Version:      version,
		ManifestHash: manifestHash,
		Integrities:  map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): digest},
	})
	if err := lockfile.WriteLockFile(dir, lf); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	// A directory where CLAUDE.md belongs makes context generation fail after
	// extensions installed successfully. Install must return that error and stop
	// before running workspace installers.
	if err := os.Mkdir(filepath.Join(dir, agentctx.ClaudeEntrypointPath), 0o755); err != nil {
		t.Fatalf("create blocking CLAUDE.md directory: %v", err)
	}
	var buf strings.Builder
	_, err = captureStdout(t, func() error {
		return Install(context.Background(), dir, wsproto.Load(dir), nil, installTestEnv(&buf, WorkspaceJobOK))
	})
	if err == nil || !strings.Contains(err.Error(), "generate AI context") {
		t.Fatalf("err = %v, want propagated context generation failure", err)
	}
	if strings.Contains(buf.String(), "Running workspace installers...") {
		t.Errorf("Install continued after context generation failed: %q", buf.String())
	}
}

func TestInstall_RunsTemplatesPhaseWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name":      "install-ws",
		"templates": []string{"alpha:1.0.0"},
	})
	hometest.Temp(t)
	t.Setenv("PUTNAMI_REGISTRY_URL", "http://127.0.0.1:1")
	// Dead registry refuses instantly; skip the retry backoff so the test
	// doesn't pay the full (1s+2s) budget waiting on a host that won't recover.
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")

	var buf strings.Builder
	testEnv := installTestEnv(&buf, WorkspaceJobOK)
	testEnv.Display.Verbose = true
	_, err := captureStdout(t, func() error {
		return Install(context.Background(), dir, wsproto.Load(dir), nil, testEnv)
	})
	// The configured template will fail to install against the dead registry,
	// so Install returns a wrapped templates-install error after printing the
	// templates phase header.
	if err == nil || !strings.Contains(err.Error(), "templates install") {
		t.Fatalf("err = %v, want templates install failure", err)
	}
	if out := buf.String(); !strings.Contains(out, "Installing templates...") {
		t.Errorf("output = %q, want templates phase header", out)
	}
}

// Install runs the toolchain pins phase once, before the workspace
// installers, which install what the lock pins: an explicit install asks it as
// explicit, the first-use install as implicit. A phase that wrote the lock is
// recorded as a lock action. A failed phase stops the install before the
// installers, with the phase's own error and next step.
func TestInstall_RunsTheToolchainPinsPhaseBeforeTheInstallers(t *testing.T) {
	hometest.Temp(t)
	var steps []string
	var phaseErr error
	run := func(ctx context.Context) ([]LifecycleAction, error) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(`{"name":"install-ws"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		env := installTestEnv(&out, WorkspaceJobOK)
		env.RunJob = func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
			steps = append(steps, "installers")
			return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
		}
		env.pinToolchains = func(_ context.Context, root string, opts toolchainpins.Options) (toolchainpins.Result, error) {
			if root != dir || opts.Hosted {
				t.Errorf("the phase ran for %s with %+v, want %s and a local run", root, opts, dir)
			}
			steps = append(steps, fmt.Sprintf("pins implicit=%v", opts.Implicit))
			return toolchainpins.Result{Written: phaseErr == nil}, phaseErr
		}
		var actions []LifecycleAction
		env.OnAction = func(action LifecycleAction) { actions = append(actions, action) }
		_, err := captureStdout(t, func() error {
			return Install(ctx, dir, wsproto.Load(dir), nil, env)
		})
		return actions, err
	}
	recorded := func(actions []LifecycleAction) bool {
		return slices.ContainsFunc(actions, func(a LifecycleAction) bool {
			return a.Kind == "lock" && a.Description == "Toolchain pins recorded"
		})
	}

	actions, err := run(context.Background())
	if err != nil {
		t.Fatalf("explicit Install: %v", err)
	}
	if want := []string{"pins implicit=false", "installers"}; !slices.Equal(steps, want) || !recorded(actions) {
		t.Fatalf("steps = %v, actions = %+v; want %v and the pins recorded", steps, actions, want)
	}

	steps = nil
	if _, err := run(shared.WithImplicitInstall(context.Background())); err != nil {
		t.Fatalf("implicit Install: %v", err)
	}
	if want := []string{"pins implicit=true", "installers"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v: the first-use install runs the phase once, before the installers", steps, want)
	}

	steps = nil
	phaseErr = protocolcli.WithNext(errors.New("toolchain pins: go: no go.work"), "putnami install")
	_, err = run(context.Background())
	if err == nil || !errors.Is(err, phaseErr) || protocolcli.SuggestedNext(err) != "putnami install" {
		t.Fatalf("Install with a failed phase = %v, want the phase's error and next step", err)
	}
	if want := []string{"pins implicit=false"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want the installers not to run after a failed phase", steps)
	}
}

const noInstallProviderFeature = "cli/first-use-path"

// writeWorkspaceWithoutExtensions writes a workspace config in dir that
// declares no extension.
func writeWorkspaceWithoutExtensions(t *testing.T, dir string) {
	t.Helper()
	config := []byte(`{"name":"install-ws","extensions":[]}`)
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), config, 0o644); err != nil {
		t.Fatal(err)
	}
}

// An explicit install in a workspace where no extension provides
// workspace-install succeeds and prints exactly one line about it, without the
// job list the old error carried.
func TestInstall_ExplicitWithoutProviderPrintsOneLine(t *testing.T) {
	spectest.Proves(t, noInstallProviderFeature, "no-install-provider-nothing-to-install", "an-explicit-command-prints-one-line-when-no-extension-provides-workspace-install")
	hometest.Temp(t)
	dir := t.TempDir()
	writeWorkspaceWithoutExtensions(t, dir)

	var buf, deps strings.Builder
	var installErr, depsErr error
	stdout, stderr := captureProcessOutput(t, func() {
		installErr = Install(context.Background(), dir, wsproto.Load(dir), nil, installTestEnv(&buf, WorkspaceJobMissing))
		depsErr = DepsInstall(context.Background(), dir, wsproto.Load(dir), "", "", installTestEnv(&deps, WorkspaceJobMissing))
	})
	if installErr != nil || depsErr != nil {
		t.Fatalf("Install = %v, DepsInstall = %v, want success", installErr, depsErr)
	}
	if got := strings.Count(buf.String(), "workspace-install"); got != 1 || !strings.Contains(buf.String(), noInstallProviderLine) {
		t.Errorf("install output = %q, want the no-provider line once", buf.String())
	}
	if want := "  " + noInstallProviderLine + "\n"; deps.String() != want {
		t.Errorf("deps install printed %q, want %q", deps.String(), want)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("explicit install leaked output: stdout %q, stderr %q", stdout, stderr)
	}
}

// The first-use install before a job command prints nothing when no extension
// provides workspace-install. It still runs the install and records the
// workspace as installed, so a later command does not run it again.
func TestInstall_ImplicitWithoutProviderPrintsNothing(t *testing.T) {
	spectest.Proves(t, noInstallProviderFeature, "no-install-provider-nothing-to-install", "an-implicit-install-prints-nothing-when-no-extension-provides-workspace-install")
	resetBootstrapEnv(t)
	hometest.Temp(t)
	dir := t.TempDir()
	writeWorkspaceWithoutExtensions(t, dir)
	if st, err := readInstallState(dir); err != nil || st != nil {
		t.Fatalf("install marker before the bootstrap = %v, %v, want none", st, err)
	}

	var installs int
	var err error
	stdout, stderr := captureProcessOutput(t, func() {
		err = EnsureWorkspaceBootstrap(context.Background(), dir, wsproto.Load(dir), BootstrapOptions{
			Command: "build",
			RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
				if req.Job == "workspace-install" {
					installs++
				}
				return WorkspaceJobResult{Outcome: WorkspaceJobMissing}, nil
			},
		})
	})
	if err != nil {
		t.Fatalf("EnsureWorkspaceBootstrap = %v, want nil", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("the implicit install printed stdout %q, stderr %q, want nothing", stdout, stderr)
	}
	if installs != 1 {
		t.Errorf("the bootstrap asked for workspace-install %d times, want 1", installs)
	}
	if st, err := readInstallState(dir); err != nil || st == nil {
		t.Errorf("install marker after the bootstrap = %v, %v, want it recorded", st, err)
	}
}

// When the runner reports that an extension did not load, the explicit install
// fails with that error, and the implicit install records no marker, so the
// next command runs the install again instead of trusting a workspace whose
// installer never ran.
func TestInstall_AFailedInstallRecordsNoInstallMarker(t *testing.T) {
	spectest.Proves(t, noInstallProviderFeature, "no-install-provider-nothing-to-install", "a-failed-install-records-no-install-marker")
	resetBootstrapEnv(t)
	hometest.Temp(t)
	dir := t.TempDir()
	writeWorkspaceWithoutExtensions(t, dir)
	notLoaded := errors.New("no loaded extension provides workspace-install; extension @acme/broken did not load: requires a newer putnami")
	runJob := func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, notLoaded
	}

	var explicitErr, implicitErr error
	var buf strings.Builder
	captureProcessOutput(t, func() {
		env := installTestEnv(&buf, WorkspaceJobFailed)
		env.RunJob = runJob
		explicitErr = Install(context.Background(), dir, wsproto.Load(dir), nil, env)
		implicitErr = EnsureWorkspaceBootstrap(context.Background(), dir, wsproto.Load(dir), BootstrapOptions{Command: "build", RunJob: runJob})
	})
	if !errors.Is(explicitErr, notLoaded) {
		t.Errorf("Install = %v, want the error that names the extension", explicitErr)
	}
	if implicitErr != nil {
		t.Errorf("EnsureWorkspaceBootstrap = %v, want the command to go on", implicitErr)
	}
	if st, err := readInstallState(dir); err != nil || st != nil {
		t.Errorf("install marker after failed installs = %v, %v, want none", st, err)
	}
}
