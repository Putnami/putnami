package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
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
			return WorkspaceJobResult{Outcome: outcome, AvailableJobs: "build, test"}, nil
		},
	}
}

// TestInstall_EmptyWorkspaceFlow exercises the primary ordering of Install on a
// workspace with no extensions/templates: extensions install (no-op success),
// AI context generation, the templates phase is skipped, and finally deps
// install runs. With no extension providing the "workspace-install" job, deps
// install returns an error which Install wraps and propagates.
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
	if err == nil || !strings.Contains(err.Error(), "deps install") {
		t.Fatalf("err = %v, want deps install failure", err)
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

// The first-use install that a command runs implicitly fills the lock pins
// its workspace files declare and the lock lacks, instead of the refresh an
// explicit install runs; its failure fails the install and names the step.
// It fills before the workspace installers, which install what the lock pins,
// and again after them, which can write a declaration. An explicit install
// never takes the fill path.
func TestInstall_TheFirstUseInstallFillsMissingToolchainPins(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "implicit-install-fills-missing-pins",
		"the-first-use-install-fills-pins-and-an-explicit-install-refreshes")
	hometest.Temp(t)
	orig := fillImplicitToolchainPins
	t.Cleanup(func() { fillImplicitToolchainPins = orig })
	var filled []string
	var fillErr error
	fillImplicitToolchainPins = func(_ context.Context, root string) (bool, error) {
		filled = append(filled, root)
		return fillErr == nil, fillErr
	}
	run := func(ctx context.Context) (string, error) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(`{"name":"install-ws"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		_, err := captureStdout(t, func() error {
			return Install(ctx, dir, wsproto.Load(dir), nil, installTestEnv(&out, WorkspaceJobOK))
		})
		return dir, err
	}

	if _, err := run(context.Background()); err != nil {
		t.Fatalf("explicit Install: %v", err)
	}
	if len(filled) != 0 {
		t.Fatalf("an explicit install took the implicit fill path: %v", filled)
	}

	implicit := shared.WithImplicitInstall(context.Background())
	dir, err := run(implicit)
	if err != nil {
		t.Fatalf("implicit Install: %v", err)
	}
	if len(filled) != 2 || filled[0] != dir || filled[1] != dir {
		t.Fatalf("implicit install filled %v, want %s before and after the installers", filled, dir)
	}

	fillErr = errors.New("metadata unreachable")
	if _, err := run(implicit); err == nil || !strings.Contains(err.Error(), "pin missing toolchains") || !errors.Is(err, fillErr) {
		t.Fatalf("implicit Install with a failed fill = %v, want the fill failure named", err)
	}
}

// An explicit install pins the toolchains the workspace declares before the
// workspace installers run, because they install the release the lock pins:
// after a go.work bump, one install pins the new release and installs it. Its
// failure stops the install before the installers and names the step.
func TestInstall_PinsTheDeclaredToolchainsBeforeTheInstallers(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "install-pins-before-installers", "explicit-install-pins-before-the-installers-run")
	hometest.Temp(t)
	origPin, origFill := pinExplicitToolchains, fillImplicitToolchainPins
	t.Cleanup(func() { pinExplicitToolchains, fillImplicitToolchainPins = origPin, origFill })
	var steps []string
	var pinErr error
	pinExplicitToolchains = func(context.Context, string) (bool, error) {
		steps = append(steps, "pin")
		return pinErr == nil, pinErr
	}
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) {
		t.Fatal("an explicit install took the implicit fill path")
		return false, nil
	}
	run := func() error {
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
		var actions []LifecycleAction
		env.OnAction = func(action LifecycleAction) { actions = append(actions, action) }
		_, err := captureStdout(t, func() error {
			return Install(context.Background(), dir, wsproto.Load(dir), nil, env)
		})
		if err == nil && !slices.ContainsFunc(actions, func(a LifecycleAction) bool {
			return a.Description == "Declared toolchain pins recorded"
		}) {
			t.Errorf("actions = %+v, want the pin recorded", actions)
		}
		return err
	}

	if err := run(); err != nil {
		t.Fatalf("explicit Install: %v", err)
	}
	if want := []string{"pin", "installers"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}

	steps = nil
	pinErr = errors.New("metadata unreachable")
	if err := run(); err == nil || !strings.Contains(err.Error(), "pin declared toolchains") || !errors.Is(err, pinErr) {
		t.Fatalf("explicit Install with a failed pin = %v, want the pin failure named", err)
	}
	if want := []string{"pin"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want the installers not to run after a failed pin", steps)
	}
}
