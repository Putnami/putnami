package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/engine"
)

// spyClientRestoreEngine returns an engine that records each request and
// returns result and err.
func spyClientRestoreEngine(result engine.SessionResult, err error) (engineRun, *[]engine.Request) {
	var requests []engine.Request
	return func(_ context.Context, req engine.Request, _ engine.EventSink) (engine.SessionResult, error) {
		requests = append(requests, req)
		return result, err
	}, &requests
}

// clientGenerateTrust is a config that sets the cache trust of
// client-generate.
func clientGenerateTrust(t *testing.T, trust string) *wsproto.Config {
	t.Helper()
	cfg := &wsproto.Config{}
	raw := fmt.Sprintf(`{"hooks":{},"options":{"client-generate":{%q:%q}}}`, cacheTrustOption, trust)
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The first-run pass runs client-generate as `putnami client-generate --all`
// would: every project, the action cache on, the cache trust that command
// resolves, and no lifecycle policy. A cache hit restores a client and a miss
// renders it.
func TestRestoreGeneratedClients_RunsAnOrdinaryCachedEngineRun(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "the-first-run-pass-runs-client-generate-cached")
	run, requests := spyClientRestoreEngine(engine.SessionResult{ExitCode: engine.ExitSuccess}, nil)
	cfg := clientGenerateTrust(t, "none")
	want := GlobalFlags{}
	if err := resolveCacheTrust(&want, cfg, nil, []string{"client-generate"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	err := restoreGeneratedClients(context.Background(), lifecycle.ClientRestoreRequest{
		WorkspaceRoot: "/ws", Config: cfg, Out: &out, Display: lifecycle.LifecycleDisplay{Verbose: true},
	}, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 {
		t.Fatalf("engine runs = %d, want 1", len(*requests))
	}
	req := (*requests)[0]
	switch {
	case req.WorkspaceRoot != "/ws" || req.Config != cfg:
		t.Errorf("workspace = %q, config = %p; want the caller's", req.WorkspaceRoot, req.Config)
	case !slices.Equal(req.Commands, []string{"client-generate"}):
		t.Errorf("commands = %v, want client-generate", req.Commands)
	case req.Global.NoCache || req.Global.NoCacheExplicit || req.Global.NoCacheProjects != "":
		t.Errorf("the run turns the cache off: %+v", req.Global)
	case req.WorkspaceLifecycle:
		t.Error("the run is a lifecycle run")
	case !req.Global.All || req.Global.Projects != "*":
		t.Errorf("selection = all %v, projects %q; want every project", req.Global.All, req.Global.Projects)
	case req.Global.CacheTrust == "" || req.Global.CacheTrust != want.CacheTrust:
		t.Errorf("cache trust = %q, want %q, the trust client-generate resolves", req.Global.CacheTrust, want.CacheTrust)
	case !req.Global.Verbose:
		t.Error("the run drops the invoking command's display")
	case req.Hooks != cfg.Hooks || req.Preflight == nil || req.Observer != nil:
		t.Errorf("hooks = %p, preflight set = %v, observer = %v", req.Hooks, req.Preflight != nil, req.Observer)
	case req.Stdout != &out || req.ErrorOut != &out:
		t.Error("the run does not write to the caller's stream")
	}
}

// A failed run is an error the bootstrap reports.
func TestRestoreGeneratedClients_ReportsAFailedRun(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "the-first-run-pass-runs-client-generate-cached")
	run, _ := spyClientRestoreEngine(engine.SessionResult{ExitCode: engine.ExitError}, nil)
	err := restoreGeneratedClients(context.Background(), lifecycle.ClientRestoreRequest{WorkspaceRoot: "/ws", Out: &bytes.Buffer{}}, run)
	if err == nil || !strings.Contains(err.Error(), "client-generate exited with code 1") {
		t.Fatalf("err = %v, want the exit code", err)
	}

	engineErr := errors.New("engine down")
	run, _ = spyClientRestoreEngine(engine.SessionResult{}, engineErr)
	if err := restoreGeneratedClients(context.Background(), lifecycle.ClientRestoreRequest{WorkspaceRoot: "/ws"}, run); !errors.Is(err, engineErr) {
		t.Fatalf("err = %v, want %v", err, engineErr)
	}

	// The run resolves the trust as client-generate does, and fails before
	// the engine when that resolution fails.
	cfg := clientGenerateTrust(t, "sometimes")
	wantErr := resolveCacheTrust(&GlobalFlags{}, cfg, nil, []string{"client-generate"})
	run, requests := spyClientRestoreEngine(engine.SessionResult{ExitCode: engine.ExitSuccess}, nil)
	err = restoreGeneratedClients(context.Background(), lifecycle.ClientRestoreRequest{WorkspaceRoot: "/ws", Config: cfg, Out: &bytes.Buffer{}}, run)
	if wantErr != nil && (err == nil || err.Error() != wantErr.Error() || len(*requests) != 0) {
		t.Fatalf("err = %v, engine runs = %d; want %v before any run", err, len(*requests), wantErr)
	}
}
